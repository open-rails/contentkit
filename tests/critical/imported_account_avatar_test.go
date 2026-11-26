package critical_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Imported accounts keep the fixed UUIDv5 ids a legacy import gave them, and
// their avatars work like any account's. Content ids stay UUIDv7.
func TestImportedAccountAvatar(t *testing.T) {
	schema := "ckia_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	w := newWorld(t, func(c *ck.Config) { c.Accounts = &ck.AccountsConfig{Schema: schema} })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		must(t, err)
	})
	a := w.apps[0]
	m := attachMedia(t, w, a, func(c *ck.Config) {
		accounts := *c.Accounts
		accounts.Avatars = &ck.AvatarsConfig{}
		c.Accounts = &accounts
	})
	user, token := importedPerson(t, w)
	client := a.client(token)
	ref := media.RefBody{Kind: "user", ID: user.ID}
	folder := layout.AccountPrefix("user", user.ID)
	files := func() int {
		objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(folder)})
		must(t, err)
		count := 0
		for _, object := range objects.Contents {
			name := aws.ToString(object.Key)
			if !strings.HasSuffix(name, "/"+layout.ManifestName) && !strings.HasSuffix(name, "/"+layout.FenceName) {
				count++
			}
		}
		return count
	}
	var served string

	t.Run("go_client_writes_and_reads", func(t *testing.T) {
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "avatar.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		ready := errors.New("avatar finished")
		watch, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		err = client.WatchMedia(watch, ref, "", func(update ck.MediaProgress) error {
			if update.Read != nil && update.Read.Status != nil && update.Read.Status.State == media.StateReady && update.Read.Status.Uploads.Completed == 1 {
				return ready
			}
			return nil
		})
		if !errors.Is(err, ready) {
			t.Fatalf("imported account's avatar was not processed: %v", err)
		}
		read, err := client.GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{})
		must(t, err)
		var url string
		for _, p := range read.Public {
			if p.Preset == "avatar" && len(p.Renditions) > 0 {
				url = p.Renditions[0].URL
			}
		}
		if !strings.Contains(url, layout.URLPrefix+layout.AccountPrefix("user", user.ID)+"public/") {
			t.Fatalf("read has no avatar rendition in the account's folder: %+v", read.Public)
		}
		if status, kind := gatewayGet(t, url); status != http.StatusOK || kind != "image/webp" {
			t.Fatalf("gateway served %d %q for the avatar", status, kind)
		}
		served = url
	})

	t.Run("shipped_typescript_client_writes_and_reads", func(t *testing.T) {
		fixture, err := json.Marshal(map[string]string{"base": a.server.URL + "/content", "token": token, "account": user.ID, "image": base64.StdEncoding.EncodeToString(imageBytes(t))})
		must(t, err)
		command := exec.CommandContext(t.Context(), "node", "--no-warnings", "imported_account_avatar.mjs")
		command.Env = append(os.Environ(), "CONTENTKIT_AVATAR_FIXTURE="+string(fixture))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("shipped client: %v\n%s", err, output)
		}
		if strings.TrimSpace(string(output)) != `{"status":"passed"}` {
			t.Fatalf("shipped client did not finish: %s", output)
		}
	})

	t.Run("content_ids_stay_uuidv7", func(t *testing.T) {
		_, err := client.IngestMedia(t.Context(), nil, media.RefBody{Kind: "gallery", ID: user.ID}, uuid.NewString(), []ck.MediaFile{{Path: "cover", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		var refused *ck.Error
		if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || refused.Code != media.CodeInvalid || !strings.Contains(refused.Message, "UUIDv7") {
			t.Fatalf("content kind took an account's UUIDv5: %v", err)
		}
	})

	t.Run("orphan_sweep_keeps_an_existing_account", func(t *testing.T) {
		config, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
		must(t, err)
		later := func() time.Time { return time.Now().Add(48 * time.Hour) }
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Manifests: mustManifests(t, m.store, registry), Now: later})
		must(t, err)
		avatar, err := registry.Ref("user", user.ID)
		must(t, err)
		if _, err := jobs.Collect(t.Context(), avatar); err != nil || files() == 0 {
			t.Fatalf("collection took the account's avatar for an orphan: %v files=%d", err, files())
		}
	})

	t.Run("account_erasure_removes_the_avatar", func(t *testing.T) {
		if served == "" || files() == 0 {
			t.Fatal("no avatar to erase")
		}
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		if n := files(); n != 0 {
			t.Fatalf("erasure left %d avatar files", n)
		}
		if status, _ := gatewayGet(t, served); status != http.StatusNotFound {
			t.Fatalf("gateway still serves the erased avatar: %d", status)
		}
	})
}

func gatewayGet(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := http.Get(url)
	must(t, err)
	response.Body.Close()
	return response.StatusCode, response.Header.Get("Content-Type")
}

// importedPerson imports an account with a fixed UUIDv5, as the legacy
// importer does, and signs it in.
func importedPerson(t *testing.T, w *world) (iam.User, string) {
	t.Helper()
	name := "legacy" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://legacy.invalid/members/"+name)).String()
	auth := w.tenant[0]
	imported, err := auth.ImportUsers(t.Context(), []iam.ImportUser{{ID: id, Email: name + "@example.com", Username: name}}, iam.ImportOptions{})
	must(t, err)
	if imported.Inserted != 1 || len(imported.Rows) != 1 || imported.Rows[0].UserID != id {
		t.Fatalf("import did not keep the UUIDv5: %+v", imported)
	}
	password, verified := authtest.Password, true
	user, err := auth.UpdateUser(t.Context(), iam.SystemIdentity(), id, iam.UserUpdate{Password: &password, EmailVerified: &verified})
	must(t, err)
	if user.ID != id || id[14] != '5' {
		t.Fatalf("account id %q is not the imported UUIDv5 %q", user.ID, id)
	}
	return user, authtest.SignIn(t, auth, authtest.User{User: user, Email: name + "@example.com", Password: password}).AccessToken
}
