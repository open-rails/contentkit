package critical_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/server"
)

func TestConfiguredStandaloneParity(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	m := attachMedia(t, w, a)
	ref := galleryRef(t, a.engine.Client(), "Configured standalone gallery")
	_, err := a.engine.Client().IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
	must(t, err)
	awaitMediaReady(t, a.engine.Client(), ref)
	_, err = a.engine.Client().PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, a.engine.Client(), ref.ID, ref.Version)
	var authConfig authkit.Config
	authtest.Replica(t, w.auth, authtest.WithConfig(func(c *authkit.Config) { authConfig = *c }))
	cfg := server.Config{Engine: a.cfg, Auth: authConfig, Authorization: &server.AuthorizationConfig{Issuer: authtest.Issuer, AppGroupID: a.cfg.Scope.AppID.String()}, Prefix: "/configured", DrainDelay: time.Millisecond, ShutdownTimeout: 5 * time.Second,
		S3: server.S3Config{Bucket: m.bucket, Endpoint: os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true}}
	cfg.Engine.Database.SkipMigrations = true
	standalone, err := server.New(t.Context(), cfg, server.Deps{Postgres: w.pool})
	must(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	base := "http://" + listener.Addr().String()
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- standalone.Serve(run, listener) }()
	finished := false
	stop := func() {
		cancel()
		if !finished {
			select {
			case err := <-done:
				must(t, err)
				finished = true
			case <-time.After(15 * time.Second):
				t.Fatal("configured standalone did not stop")
			}
		}
		must(t, standalone.Close(context.Background()))
	}
	t.Cleanup(stop)
	waitFor(t, func() bool {
		response, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 204 || response.StatusCode == 200
	})
	client := ck.NewClient(base+"/configured", nil)
	t.Run("CK-J01-S06_configured_media_and_ClickHouse_work_through_standalone", func(t *testing.T) {
		w.clock.set(time.Now().UTC())
		left, err := a.client("").GetMediaPresets(t.Context())
		must(t, err)
		right, err := client.GetMediaPresets(t.Context())
		must(t, err)
		if len(left) == 0 || !reflect.DeepEqual(left, right) {
			t.Fatalf("configured preset contract diverged: %+v / %+v", left, right)
		}
		local, err := a.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		remote, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		if len(local.Files) != 1 || len(remote.Files) != 1 || remote.Files[0].Path != "reader/one.webp" || remote.Files[0].W != 48 || remote.Files[0].H != 32 || local.Files[0].Path != remote.Files[0].Path {
			t.Fatalf("configured media read diverged: %+v / %+v", local, remote)
		}
		assertImage(t, remote.Files[0].URL, 48, 32)
		persona, err := w.auth.Persona("app")
		must(t, err)
		key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Standalone editor", Role: persona.OwnerRole()})
		must(t, err)
		staff := ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{key.Secret, http.DefaultTransport}})
		editor, err := staff.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(editor.Uploads) == 0 || editor.State == "" {
			t.Fatal("standalone did not resolve real app-key editor authority")
		}
		foreign, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), w.apps[1].group, iam.NewAPIKey{Name: "Foreign standalone editor", Role: persona.OwnerRole()})
		must(t, err)
		_, err = ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{foreign.Secret, http.DefaultTransport}}).GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		requireError(t, err, 403, "forbidden")
		eventID := uuid.NewString()
		must(t, client.PostSignals(t.Context(), discovery.Signals{Session: uuid.NewString(), Signals: []discovery.SignalInput{{ContentID: ref.ID, Type: "click", EventID: eventID, OccurredAt: time.Now()}}}))
		rows, err := a.deps.ClickHouse.Query(t.Context(), "SELECT count() FROM "+a.cfg.ClickHouse.Database+".signals FINAL WHERE tenant=? AND event_id=?", a.cfg.Scope.AppID.String(), eventID)
		must(t, err)
		var count uint64
		if !rows.Next() {
			t.Fatal("standalone signal query had no count")
		}
		must(t, rows.Scan(&count))
		must(t, rows.Close())
		if count != 1 {
			t.Fatalf("configured standalone lost/duplicated actual ClickHouse write: %d", count)
		}
		stop()
		must(t, w.pool.Ping(t.Context()))
		must(t, a.deps.ClickHouse.Exec(t.Context(), "SELECT 1"))
		assertImage(t, local.Files[0].URL, 48, 32)
	})
}
