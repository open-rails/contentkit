package critical_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/catalogstore"
	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/internal/search"
	"github.com/open-rails/contentkit/internal/worker"
	"github.com/open-rails/contentkit/kinds"
)

// An API and a worker starting together on fresh storage run two first search
// syncs at once: the worker's scheduled run and the one the API's startup
// write queued. Both create the same backfill positions.
func TestConcurrentFirstSearchSync(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"} })
	api := w.apps[0]

	t.Run("CK-J07-S24_first_search_syncs_of_an_api_and_worker_starting_together_both_succeed", func(t *testing.T) {
		ctx := t.Context()
		worker, err := ck.New(ctx, api.cfg, api.deps)
		must(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := worker.Close(ctx); err != nil {
				t.Errorf("close worker: %v", err)
			}
		})
		tenant := publicationTenant(api)
		backfill := pgx.Identifier{w.schema, "content_search_backfill"}.Sanitize()
		jobs := pgx.Identifier{publicationRiverSchema(api), "river_job"}.Sanitize()

		// The gate holds the first backfill position until both runs wait on
		// it, then rolls back: the runs race to create it, the interleaving a
		// concurrent start otherwise leaves to timing.
		gate, err := w.pool.Begin(ctx)
		must(t, err)
		t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
		var pid int
		must(t, gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
		_, err = gate.Exec(ctx, `INSERT INTO `+backfill+` (tenant_id, content_kind, language) VALUES ($1, 'article', 'en')`, tenant)
		must(t, err)

		must(t, api.engine.Start(ctx))
		must(t, worker.StartJobs(ctx))
		// Like a host's startup marker entity.
		_, err = api.engine.Client().PostEntities(ctx, []entities.EntityInput{{Kind: "tag", Slug: "restricted", Internal: true,
			Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Restricted"}}}})
		must(t, err)
		waitForWithin(t, 30*time.Second, func() bool {
			var waiting int
			must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting))
			return waiting == 2
		})
		must(t, gate.Rollback(ctx))

		waitForWithin(t, 30*time.Second, func() bool {
			rows, err := w.pool.Query(ctx, `SELECT id, state, errors::text FROM `+jobs+` WHERE kind = 'contentkit_search_sync' AND errors IS NOT NULL`)
			must(t, err)
			failed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
				var id int64
				var state, errs string
				err := r.Scan(&id, &state, &errs)
				return fmt.Sprintf("job %d %s: %s", id, state, errs), err
			})
			must(t, err)
			if len(failed) > 0 {
				t.Fatalf("a concurrent first search sync failed:\n%s", strings.Join(failed, "\n"))
			}
			var runs, open int
			must(t, w.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state <> 'completed') FROM `+jobs+` WHERE kind = 'contentkit_search_sync'`).Scan(&runs, &open))
			return runs >= 2 && open == 0
		})

		rows, err := w.pool.Query(ctx, `SELECT content_kind || '/' || language || ':' || state FROM `+backfill+` WHERE tenant_id = $1 ORDER BY 1`, tenant)
		must(t, err)
		positions, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		want := "article/de:done article/en:done gallery/de:done gallery/en:done tag/de:done tag/en:done"
		if got := strings.Join(positions, " "); got != want {
			t.Fatalf("backfill positions %s, want %s", got, want)
		}
	})
}

type searchTuple struct {
	Location, Revision string
	Updated            time.Time
}

func readSearchTuple(t *testing.T, w *world, key search.DocumentKey) searchTuple {
	t.Helper()
	var row searchTuple
	must(t, w.pool.QueryRow(t.Context(), `SELECT ctid::text,xmin::text,updated_at FROM `+pgx.Identifier{w.schema, "content_search_documents"}.Sanitize()+`
 WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id IS NOT DISTINCT FROM $4 AND language=$5`,
		key.TenantID, key.ContentKind, key.ContentID, key.ContentVersionID, key.Language).Scan(&row.Location, &row.Revision, &row.Updated))
	return row
}

func TestKeywordDocumentWrites(t *testing.T) {
	w := newWorld(t)
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%t", versioned), func(t *testing.T) {
			ref := contentref.New(publicationTenant(w.apps[0]), "tag", contentref.NewID())
			if versioned {
				ref = contentref.NewVersion(ref.TenantID, "gallery", ref.ContentID, contentref.NewID())
			}
			doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: ref, Language: "en"}, Title: "First title"}
			write := func() {
				t.Helper()
				must(t, search.UpsertKeywordDocuments(t.Context(), w.pool, w.schema, []search.KeywordDocument{doc}))
			}
			write()
			before := readSearchTuple(t, w, doc.DocumentKey)
			doc.Title, doc.Aliases, doc.Keywords = "  First title  ", []string{}, []string{}
			write()
			if after := readSearchTuple(t, w, doc.DocumentKey); after != before {
				t.Fatalf("equal normalized text rewrote the tuple: before=%+v after=%+v", before, after)
			}
			doc.Title = "First title"
			for _, field := range []string{"title", "aliases", "keywords"} {
				switch field {
				case "title":
					doc.Title = "Changed title"
				case "aliases":
					doc.Aliases = []string{"New alias"}
				case "keywords":
					doc.Keywords = []string{"New keyword"}
				}
				write()
				after := readSearchTuple(t, w, doc.DocumentKey)
				if after.Location == before.Location || after.Revision == before.Revision || !after.Updated.After(before.Updated) {
					t.Fatalf("changed %s did not update the tuple and timestamp: before=%+v after=%+v", field, before, after)
				}
				var title string
				var aliases, keywords []string
				must(t, w.pool.QueryRow(t.Context(), `SELECT title,aliases,keywords FROM `+pgx.Identifier{w.schema, "content_search_documents"}.Sanitize()+` WHERE content_id=$1`, ref.ContentID).Scan(&title, &aliases, &keywords))
				if title != doc.Title || !slices.Equal(aliases, doc.Aliases) || !slices.Equal(keywords, doc.Keywords) {
					t.Fatalf("stored text = %q %q %q, want %+v", title, aliases, keywords, doc)
				}
				write()
				if got := readSearchTuple(t, w, doc.DocumentKey); got != after {
					t.Fatalf("retry after changing %s rewrote the tuple: before=%+v after=%+v", field, after, got)
				}
				before = after
			}
			doc.Title = " "
			write()
			var n int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "content_search_documents"}.Sanitize()+` WHERE content_id=$1`, ref.ContentID).Scan(&n))
			if n != 0 {
				t.Fatalf("empty title kept %d documents", n)
			}
		})
	}
}

// The source reads real catalog text, then lets a catalog edit commit before
// the worker can publish that snapshot.
type searchBuildHook struct {
	worker.Source
	afterBuild func(context.Context) error
}

func (s *searchBuildHook) BuildDocuments(ctx context.Context, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	docs, err := s.Source.BuildDocuments(ctx, kind, language, refs)
	if err == nil && s.afterBuild != nil {
		err = s.afterBuild(ctx)
	}
	return docs, err
}

func TestSearchDocumentRebuilds(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
		gallery := cfg.Content.Kinds["gallery"]
		gallery.Entities = []string{"tag"}
		cfg.Content.Kinds["gallery"] = gallery
	})
	a := w.apps[0]
	publicationOnlyFleet(t, a)
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Original title")
	_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	awaitPublicationJobsIdle(t, w, a)
	key := search.DocumentKey{ContentRef: contentref.NewVersion(publicationTenant(a), ref.Kind, ref.ID, ref.Version), Language: "en"}
	q := func(table string) string { return pgx.Identifier{w.schema, table}.Sanitize() }
	run := func() {
		t.Helper()
		for n := 0; ; n++ {
			more, err := jobhook.SearchSync(t.Context(), a.engine)
			must(t, err)
			if !more {
				return
			}
			if n == 20 {
				t.Fatal("search sync did not settle")
			}
		}
	}
	pending := func() int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+q("content_search_dirty")+` WHERE content_id=$1`, ref.ID).Scan(&n))
		return n
	}
	mark := func() {
		t.Helper()
		must(t, search.MarkDirty(t.Context(), w.pool, w.schema, []search.DirtyMark{{DocumentKey: key, Reason: "catalog"}}))
	}
	run()
	before := readSearchTuple(t, w, key)
	mark()
	run()
	if after := readSearchTuple(t, w, key); after != before || pending() != 0 {
		t.Fatalf("unchanged rebuild: before=%+v after=%+v pending=%d", before, after, pending())
	}
	patch := func(title string, aliases ...string) error {
		_, err := admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: title, Aliases: aliases}}})
		return err
	}
	must(t, patch("Changed title"))
	if pending() == 0 {
		t.Fatal("title edit did not mark search")
	}
	run()
	if after := readSearchTuple(t, w, key); after == before {
		t.Fatal("title edit did not rewrite the document")
	}
	must(t, patch("Changed title", "Changed alias"))
	run()
	var aliases []string
	must(t, w.pool.QueryRow(t.Context(), `SELECT aliases FROM `+q("content_search_documents")+` WHERE content_id=$1`, ref.ID).Scan(&aliases))
	if !slices.Contains(aliases, "Changed alias") {
		t.Fatalf("alias edit did not reach search: %q", aliases)
	}
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "keyword", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Linked keyword"}}}})
	must(t, err)
	_, err = admin.PutContentEntitiesShared(t.Context(), ref.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tags[0].ID}}}, ck.PutContentEntitiesSharedQuery{})
	must(t, err)
	if pending() == 0 {
		t.Fatal("membership edit did not mark search")
	}
	run()
	var keywords []string
	must(t, w.pool.QueryRow(t.Context(), `SELECT keywords FROM `+q("content_search_documents")+` WHERE content_id=$1`, ref.ID).Scan(&keywords))
	if !slices.Contains(keywords, "Linked keyword") {
		t.Fatalf("entity membership did not reach search: %q", keywords)
	}

	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	identity, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = w.pool }))
	source, err := catalogstore.New(t.Context(), catalogstore.Options{Pool: w.pool, Schema: w.schema, Tenant: key.TenantID, Kinds: a.cfg.Content, Auth: identity.Authenticator(), Now: w.clock.now})
	must(t, err)
	t.Cleanup(source.Close)
	mark()
	var revision int64
	must(t, w.pool.QueryRow(t.Context(), `SELECT revision FROM `+q("content_search_dirty")+` WHERE content_id=$1`, ref.ID).Scan(&revision))
	before = readSearchTuple(t, w, key)
	gated := &searchBuildHook{Source: source, afterBuild: func(context.Context) error { return patch("Newer title", "Changed alias") }}
	_, err = worker.SyncOnce(t.Context(), worker.Options{Pool: w.pool, Schema: w.schema, Tenant: key.TenantID, SupportedLanguages: a.cfg.Content.Languages, Sources: []worker.Source{gated}})
	must(t, err)
	var newer int64
	must(t, w.pool.QueryRow(t.Context(), `SELECT revision FROM `+q("content_search_dirty")+` WHERE content_id=$1`, ref.ID).Scan(&newer))
	if newer <= revision || readSearchTuple(t, w, key) != before {
		t.Fatalf("stale rebuild lost newer mark or changed its document: revision=%d newer=%d", revision, newer)
	}
	run()
	var title string
	must(t, w.pool.QueryRow(t.Context(), `SELECT title FROM `+q("content_search_documents")+` WHERE content_id=$1`, ref.ID).Scan(&title))
	if title != "Newer title" || pending() != 0 {
		t.Fatalf("newer rebuild: title=%q pending=%d", title, pending())
	}
	_, err = admin.PostContentUnpublish(t.Context(), ref.ID)
	must(t, err)
	awaitPublicationJobsIdle(t, w, a)
	run()
	var documents int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+q("content_search_documents")+` WHERE content_id=$1`, ref.ID).Scan(&documents))
	if documents != 0 || pending() != 0 {
		t.Fatalf("withdrawal kept documents=%d pending=%d", documents, pending())
	}
}
