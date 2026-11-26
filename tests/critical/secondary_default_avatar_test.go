package critical_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/worker"
)

// An account kind's defaults are account media: the avatar primary publishes
// them into the accounts area of its bucket and the tenant account cache, and
// every site shows them at its own media host, whose route's accounts names
// the primary's bucket.
func TestSecondaryDefaultAvatar(t *testing.T) {
	stockWorker(t)
	schema := "cksd_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	w := newWorld(t, func(c *ck.Config) { c.Accounts = &ck.AccountsConfig{Schema: schema} })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		must(t, err)
	})
	primary, secondary := w.apps[0], w.apps[1] // one tenant
	avatars := func(a *ck.AvatarsConfig) func(*ck.Config) {
		return func(c *ck.Config) {
			accounts := *c.Accounts
			accounts.Avatars = a
			c.Accounts = &accounts
		}
	}
	pm := attachMedia(t, w, primary, avatars(&ck.AvatarsConfig{}))
	// Hosts sharing a database never share a media worker schema.
	secondaryWorker := w.schema + "_secondary_worker"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{secondaryWorker}.Sanitize()+" CASCADE")
		must(t, err)
	})
	sm := attachMedia(t, w, secondary, avatars(&ck.AvatarsConfig{Primary: primary.server.URL + "/content", Key: "unused-by-reads"}),
		func(c *ck.Config) { c.Media.WorkerSchema = secondaryWorker })
	accountStorage, _, err := primary.cfg.AccountMedia()
	must(t, err)
	kindsFile := func(t *testing.T, a *application) string {
		t.Helper()
		kinds, err := a.cfg.MediaRegistry()
		must(t, err)
		path := filepath.Join(t.TempDir(), "kinds.json")
		must(t, os.WriteFile(path, kinds, 0o600))
		return path
	}
	accountKeys := func(t *testing.T, bucket, prefix string) []string {
		t.Helper()
		objects, err := pm.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &bucket, Prefix: aws.String(prefix)})
		must(t, err)
		var keys []string
		for _, object := range objects.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		return keys
	}
	defaults := layout.AccountPrefix("user", layout.DefaultID)
	database := "DATABASE_URL=" + os.Getenv("CONTENTKIT_CRITICAL_PG")

	t.Run("CK-J12-S11_account_defaults_need_the_account_cache", func(t *testing.T) {
		logs, err := publishDefaults(t, kindsFile(t, primary), pm.bucket)
		if err == nil || !strings.Contains(logs, "MEDIA_ACCOUNT_SCHEMA") {
			t.Fatalf("the primary published account defaults no site but itself can read: %v\n%s", err, logs)
		}
		if keys := accountKeys(t, pm.bucket, defaults); len(keys) != 0 {
			t.Fatalf("a refused publication wrote %v", keys)
		}
	})

	t.Run("CK-J12-S11_only_the_primarys_bucket_holds_them", func(t *testing.T) {
		out, err := publishDefaults(t, kindsFile(t, primary), pm.bucket, database, "MEDIA_ACCOUNT_SCHEMA="+accountStorage.Schema)
		if err != nil || strings.TrimSpace(out) != "user" {
			t.Fatalf("primary publish-defaults: %v\n%s", err, out)
		}
		out, err = publishDefaults(t, kindsFile(t, secondary), sm.bucket)
		if err != nil || strings.TrimSpace(out) != "" {
			t.Fatalf("secondary publish-defaults: %v\n%s", err, out)
		}
		if keys := accountKeys(t, pm.bucket, defaults+layout.ManifestName); len(keys) != 1 {
			t.Fatalf("the primary's bucket has no %s: %v", defaults, keys)
		}
		if keys := accountKeys(t, sm.bucket, layout.AccountRoot); len(keys) != 0 {
			t.Fatalf("the secondary's bucket holds account media: %v", keys)
		}
		c, mc := secondary.cfg, *secondary.cfg.Media
		mc.Kinds = append(slices.Clone(mc.Kinds), avatar.User())
		c.Media = &mc
		if _, err := c.MediaKinds(); err == nil {
			t.Fatal("a site declared the account kind as its own media")
		}
	})

	t.Run("CK-J12-S11_a_secondary_shows_the_default_through_its_gateway", func(t *testing.T) {
		served := func(t *testing.T, url string, width int) {
			t.Helper()
			if !strings.HasPrefix(url, sm.gateway.URL+layout.URLPrefix+defaults+layout.AreaPublic+"/") {
				t.Fatalf("default %s is not the account default at the secondary's media host %s", url, sm.gateway.URL)
			}
			response, err := http.Get(url)
			must(t, err)
			data, err := io.ReadAll(response.Body)
			must(t, errors.Join(err, response.Body.Close()))
			config, format, err := decodeImageConfig(data)
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/webp" || err != nil || format != "webp" || config.Width != width {
				t.Fatalf("the secondary's gateway served %s as %d %q %s %dpx (%v), want a %dpx WebP", url, response.StatusCode, response.Header.Get("Content-Type"), format, config.Width, err, width)
			}
		}
		model, err := secondary.client("").GetKinds(t.Context())
		must(t, err)
		if model.Avatars == nil || model.Avatars.DefaultImage == nil || len(model.Avatars.DefaultImage.Renditions) != len(avatar.User().Public[0].Widths) {
			t.Fatalf("the secondary's content model has no default avatar: %+v", model.Avatars)
		}
		image := *model.Avatars.DefaultImage
		for _, r := range image.Renditions {
			served(t, r.URL, r.W)
		}

		// A person without an avatar is shown with it beside what they wrote.
		_, token := w.person(t, 0)
		item := publishedArticle(t, secondary, "default avatar")
		comment, err := secondary.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "No avatar yet"}, ck.PostContentCommentsQuery{})
		must(t, err)
		got := readComment(t, secondary, item.ID, comment.ID)
		if got.Author == nil || got.Author.Avatar == "" || got.Author.AvatarSrcSet != image.SrcSet() {
			t.Fatalf("the author has no default avatar: %+v", got.Author)
		}
		for _, r := range image.Renditions {
			if r.URL == got.Author.Avatar {
				served(t, r.URL, r.W)
				return
			}
		}
		t.Fatalf("the author's avatar %s is none of the default's %+v", got.Author.Avatar, image.Renditions)
	})

	t.Run("CK-J12-S11_rebuild_restores_them", func(t *testing.T) {
		table := pgx.Identifier{accountStorage.Schema, "content_media_items"}.Sanitize()
		recorded := func() string {
			var images string
			err := w.pool.QueryRow(t.Context(), "SELECT images::text FROM "+table+" WHERE content_kind='user' AND content_id=$1", layout.DefaultID).Scan(&images)
			if errors.Is(err, pgx.ErrNoRows) {
				return ""
			}
			must(t, err)
			return images
		}
		before := recorded()
		if before == "" || before == "[]" {
			t.Fatalf("the account default is not recorded: %q", before)
		}
		_, err := w.pool.Exec(t.Context(), "DELETE FROM "+table+" WHERE content_kind='user' AND content_id=$1", layout.DefaultID)
		must(t, err)
		_, err = worker.Rebuild(t.Context(), pm.workerConfig)
		must(t, err)
		if after := recorded(); after != before {
			t.Fatalf("rebuild recorded the account default as %q, want %q", after, before)
		}
	})
}
