package critical_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// An embedded host serves from API processes, which only Start, and works
// ContentKit's jobs in its worker process, which calls StartJobs.
func TestServingAndWorkingProcesses(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J01-S09_api_process_enqueues_and_worker_process_works_jobs", func(t *testing.T) {
		must(t, a.engine.Start(t.Context()))
		must(t, a.engine.Ready(t.Context()))
		admin, reader := a.engine.Client(), a.client("")
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"},
			Text: map[string]catalog.VersionText{"en": {Title: "Worked elsewhere", Body: "Published by the worker process."}}}}})
		must(t, err)
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		release := currentReleaseJob(t, a, item.Versions[0].ID)
		jobs := pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()
		pending := func() int {
			var n int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_version_release' AND args->>'id'=$1
 AND state IN ('available','scheduled','retryable')`, release.ID).Scan(&n))
			return n
		}
		// No process works jobs: the release waits, durable, in ContentKit's River.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			if err == nil && got.Live {
				t.Fatal("a serving-only engine worked the publication job")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if pending() != 1 {
			t.Fatal("the serving engine did not enqueue the release")
		}
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		worker, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			must(t, worker.Close(ctx))
		})
		must(t, worker.StartJobs(t.Context()))
		must(t, worker.Ready(t.Context()))
		if err := worker.StartJobs(t.Context()); err == nil {
			t.Fatal("a second StartJobs started another worker owner")
		}
		waitForWithin(t, 30*time.Second, func() bool {
			got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			return err == nil && got.Live && got.Body == "Published by the worker process."
		})
		if pending() != 0 {
			t.Fatal("the worker left the release pending")
		}
	})
}

// A remote host delivers AuthKit's account lifecycle and resolves its
// channels through the HTTP client, as a host service holding an API key.
func TestHostServiceSurface(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.Channels = &kinds.Channels{Creation: "staff", Route: "channels"}
		k := cfg.Content.Kinds["article"]
		k.Channel = &kinds.ChannelPolicy{Fixed: "blog"}
		k.AccessLevels = []string{"public"}
		cfg.Content.Kinds["article"] = k
	})
	a := w.apps[0]
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg, ck.MigrationOptions{Scopes: a.deps.Scopes}))
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "host service", Role: persona.OwnerRole()})
	must(t, err)
	host := a.client(key.Secret)

	t.Run("CK-J31-S05_account_lifecycle_over_http", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "remote lifecycle")
		comment, err := a.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Delivered remotely", Language: "en"}, ck.PostContentCommentsQuery{})
		must(t, err)
		_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		must(t, host.PutAccountDeleted(t.Context(), user.ID, content.AccountDeleted{Deleted: true}))
		if got := readComment(t, a, item.ID, comment.ID); got.UserID != "" || got.Author != nil || got.AnonName != "<Deleted>" {
			t.Fatalf("remote soft deletion exposed attribution: %+v", got)
		}
		must(t, host.PutAccountDeleted(t.Context(), user.ID, content.AccountDeleted{Deleted: false}))
		if got := readComment(t, a, item.ID, comment.ID); got.UserID != user.ID || got.Author == nil {
			t.Fatalf("remote restore did not recover attribution: %+v", got)
		}
		must(t, host.PostAccountAnonymize(t.Context(), user.ID))
		must(t, host.PostAccountAnonymize(t.Context(), user.ID))
		got := readComment(t, a, item.ID, comment.ID)
		if got.UserID != "" || got.Author != nil || got.Body != "Delivered remotely" {
			t.Fatalf("remote anonymization lost discussion or kept attribution: %+v", got)
		}
		state, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if state.Count == nil || *state.Count != 1 {
			t.Fatalf("remote anonymization destroyed aggregate contribution: %+v", state)
		}
		must(t, host.PutAccountDeleted(t.Context(), user.ID, content.AccountDeleted{Deleted: false}))
		if got := readComment(t, a, item.ID, comment.ID); got.UserID != "" || got.Author != nil {
			t.Fatal("remote restore relinked an anonymized account")
		}
		_, member := w.person(t, 0)
		err = a.client(member).PostAccountAnonymize(t.Context(), user.ID)
		requireError(t, err, 403, "forbidden")
	})

	t.Run("CK-J31-S06_channel_management_read_names_scope_and_owner", func(t *testing.T) {
		page, err := host.GetManageChannels(t.Context(), ck.GetManageChannelsQuery{Slug: "blog", Limit: 2})
		must(t, err)
		if len(page.Channels) != 1 || page.Channels[0].Slug != "blog" {
			t.Fatalf("blog lookup = %+v", page.Channels)
		}
		blog, err := host.GetManageChannel(t.Context(), string(page.Channels[0].ID), ck.GetManageChannelQuery{})
		must(t, err)
		if blog.AuthScope == "" || blog.OwnerUserID != "" || blog.Slug != "blog" {
			t.Fatalf("management read hid the site channel's scope or owner: %+v", blog)
		}
		if _, err := a.client("").GetManageChannel(t.Context(), string(blog.ID), ck.GetManageChannelQuery{}); err == nil {
			t.Fatal("an anonymous reader reached the management read")
		}
	})
}

// Reconcile repairs derived catalog state after a host changes its model.
func TestEngineReconcile(t *testing.T) {
	model := func(path bool) func(*ck.Config) {
		return func(cfg *ck.Config) {
			cfg.Content.EntityKinds["series"] = kinds.EntityKind{}
			k := cfg.Content.Kinds["article"]
			k.Entities = []string{"series"}
			if path {
				k.PathEntity = "series"
			}
			cfg.Content.Kinds["article"] = k
		}
	}
	w := newWorld(t, model(false))
	a := w.apps[0]
	t.Run("CK-J31-S07_reconcile_rederives_paths_after_a_model_change", func(t *testing.T) {
		admin := a.engine.Client()
		series, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "series", Slug: "saga", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Saga"}}}})
		must(t, err)
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"},
			Text: map[string]catalog.VersionText{"en": {Title: "Chapter One"}}}}})
		must(t, err)
		_, err = admin.PutContentEntities(t.Context(), item.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: series[0].ID}}}, ck.PutContentEntitiesQuery{})
		must(t, err)
		before, err := admin.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if before.Path == "" || strings.Contains(before.Path, "/saga/") {
			t.Fatalf("path before path_entity = %q", before.Path)
		}
		a.server.Close()
		must(t, a.engine.Close(t.Context()))
		cfg := a.cfg
		model(true)(&cfg)
		cfg.Database.SkipMigrations = true
		reopened, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		a.engine, a.server = reopened, mountEngine(t, reopened)
		stale, err := reopened.Client().GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if stale.Path != before.Path {
			t.Fatalf("path changed before Reconcile: %q", stale.Path)
		}
		got, err := reopened.Reconcile(t.Context())
		must(t, err)
		if got.Facets < 1 || got.Paths < 1 {
			t.Fatalf("Reconcile visited %+v", got)
		}
		after, err := reopened.Client().GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if want := strings.Replace(before.Path, "/chapter-one", "/saga/chapter-one", 1); !strings.HasSuffix(before.Path, "/chapter-one") || after.Path != want {
			t.Fatalf("path after Reconcile = %q; want %q", after.Path, want)
		}
		again, err := reopened.Reconcile(t.Context())
		must(t, err)
		if again.Paths != got.Paths {
			t.Fatalf("Reconcile is not idempotent: %+v then %+v", got, again)
		}
	})
}
