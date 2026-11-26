package critical_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/subject"
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
	cfg := server.Config{Engine: a.cfg, Auth: authConfig, Authorization: &server.AuthorizationConfig{Issuer: authtest.Issuer, AppGroupID: a.cfg.Scope.AppID.String()}, Prefix: "/configured", DrainDelay: time.Millisecond, ShutdownTimeout: 5 * time.Second, ReadTimeout: 2 * time.Second,
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
		staff := ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{token: key.Secret, base: http.DefaultTransport}})
		editor, err := staff.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(editor.Uploads) == 0 || editor.State == "" {
			t.Fatal("standalone did not resolve real app-key editor authority")
		}
		foreign, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), w.apps[1].group, iam.NewAPIKey{Name: "Foreign standalone editor", Role: persona.OwnerRole()})
		must(t, err)
		_, err = ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{token: foreign.Secret, base: http.DefaultTransport}}).GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		requireError(t, err, 403, "forbidden")
		visitor := uuid.NewString()
		browser := ck.NewClient(base+"/configured", &http.Client{Transport: readerTransport{test: t, w: w, anonymous: visitor}})
		must(t, browser.PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{{Type: discovery.ActivityOpen, ContentID: ref.ID}}}))
		who, _ := subject.Anonymous(uuid.UUID(a.cfg.Scope.AppID), visitor)
		waitForWithin(t, 30*time.Second, func() bool {
			return logged(t, a.deps.ClickHouse, a.cfg.ClickHouse.Database, a.cfg.Scope.AppID.String(), who) > 0
		})
		if n := logged(t, a.deps.ClickHouse, a.cfg.ClickHouse.Database, a.cfg.Scope.AppID.String(), who); n != 1 {
			t.Fatalf("configured standalone lost/duplicated actual ClickHouse write: %d", n)
		}
		must(t, w.pool.Ping(t.Context()))
		must(t, a.deps.ClickHouse.Exec(t.Context(), "SELECT 1"))
		assertImage(t, local.Files[0].URL, 48, 32)
	})
	t.Run("CK-J01-S13_signal_reads_serving_a_request_carry_a_deadline_and_memory_ceiling", func(t *testing.T) {
		staff := ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{token: staffKey(t, w, a), base: http.DefaultTransport}})
		_, err := staff.GetStatsActive(t.Context(), ck.GetStatsActiveQuery{Days: 7})
		must(t, err)
		must(t, a.deps.ClickHouse.Exec(t.Context(), "SYSTEM FLUSH LOGS"))
		rows, err := a.deps.ClickHouse.Query(t.Context(), `SELECT Settings['max_memory_usage'], toUInt64OrZero(Settings['max_execution_time']) FROM system.query_log
 WHERE type = 'QueryFinish' AND event_time > now() - 600 AND has(databases, ?) AND position(query, 'audience_daily') > 0 AND query_kind = 'Select'`, a.cfg.ClickHouse.Database)
		must(t, err)
		reads := 0
		for rows.Next() {
			var memory string
			var seconds uint64
			must(t, rows.Scan(&memory, &seconds))
			if memory != "1073741824" || seconds == 0 || seconds > 20 {
				t.Errorf("an unbounded request read: max_memory_usage=%q max_execution_time=%d", memory, seconds)
			}
			reads++
		}
		must(t, rows.Close())
		if reads == 0 {
			t.Fatal("the active-days read issued no ClickHouse query")
		}
	})
	t.Run("CK-J01-S10_the_server_bounds_bodies_and_headers_keeps_streams_open_and_marks_nosniff", func(t *testing.T) {
		for _, path := range []string{"/readyz", "/configured/v1/kinds", "/configured/v1/no-such-route", "/elsewhere"} {
			if _, _, header := answer(t, http.MethodGet, base+path, nil, nil); header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s: %v", path, header)
			}
		}
		if status, _, _ := answer(t, http.MethodGet, base+"/configured/v1/kinds", http.Header{"X-Filler": {strings.Repeat("a", 80<<10)}}, nil); status != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("an 80 KiB header block: %d", status)
		}
		// A body that stalls is cut at the read timeout and answered.
		conn, err := net.Dial("tcp", listener.Addr().String())
		must(t, err)
		defer conn.Close()
		start := time.Now()
		_, err = fmt.Fprintf(conn, "POST /configured/v1/activity HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", listener.Addr())
		must(t, err)
		must(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
		stalled, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("a stalled body was never cut: %v", err)
		}
		stalled.Body.Close()
		if waited := time.Since(start); stalled.StatusCode != http.StatusBadRequest || waited < time.Second || waited > 10*time.Second {
			t.Fatalf("stalled body: %d after %s", stalled.StatusCode, waited)
		}
		// An event stream outlives the read timeout: its heartbeat comes
		// after ten seconds.
		staff := ck.NewClient(base+"/configured", &http.Client{Transport: bearerTransport{token: staffKey(t, w, a), base: http.DefaultTransport}})
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var events []string
		opened := time.Now()
		heartbeat := errors.New("heartbeat")
		err = staff.WatchMedia(ctx, ref, "", func(p ck.MediaProgress) error {
			if events = append(events, p.Event); p.Event == "heartbeat" {
				return heartbeat
			}
			return nil
		})
		if !errors.Is(err, heartbeat) || events[0] != "snapshot" || time.Since(opened) < 2*time.Second {
			t.Fatalf("event stream: %v after %s, events %v", err, time.Since(opened), events)
		}
	})
	stop()
	must(t, a.deps.ClickHouse.Exec(t.Context(), "SELECT 1"))
	read, err := a.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	assertImage(t, read.Files[0].URL, 48, 32)
}
