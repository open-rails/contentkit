package critical_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/migrations"
	"github.com/open-rails/contentkit/kinds"
)

// preferenceResync is the engine's preference job asked to re-send every row.
type preferenceResync struct {
	Resync bool `json:"resync"`
}

func (preferenceResync) Kind() string { return "contentkit_preference_sync" }

// TestFavoritesMigrateToBinaryKeys puts favorites back in their text-keyed
// table, as the previous release left them, writes to it as that release's
// code would between the migration's steps, and checks the conversion keeps
// every row and that toggle, card state, recount, preference sync and
// rankings read the converted rows.
func TestFavoritesMigrateToBinaryKeys(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Interactions.Limits.Disabled = true
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", Popular: &kinds.EntityPopularity{ContentKind: "article"}}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag"}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	admin := a.engine.Client()
	app := a.cfg.Scope.AppID.String()
	ctx := t.Context()
	table := func(name string) string { return pgx.Identifier{w.schema, name}.Sanitize() }
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := w.pool.Exec(ctx, query, args...)
		must(t, err)
	}

	t.Run("CK-J01-S17_favorites_convert_to_binary_keys_and_stay_readable", func(t *testing.T) {
		// Favorites written by the release before the conversion.
		item := publishedArticle(t, a, "Favorited before the migration")
		version := item.Versions[0].ID
		u1, token1 := w.person(t, 0)
		u2, token2 := w.person(t, 0)
		_, token3 := w.person(t, 0)
		u4, token4 := w.person(t, 0)
		u5, token5 := w.person(t, 0)
		for _, token := range []string{token1, token2, token3, token4, token5} {
			_, err := a.client(token).PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{})
			must(t, err)
		}
		_, err := a.client(token1).PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{Version: version})
		must(t, err)
		_, err = a.client(token3).DeleteContentFavorite(ctx, item.ID, ck.DeleteContentFavoriteQuery{})
		must(t, err)
		must(t, a.engine.AnonymizeAccount(ctx, u4.ID))
		tag := func(slug string) entities.EntityID {
			created, err := admin.PostEntities(ctx, []entities.EntityInput{{Kind: "tag", Slug: slug}})
			must(t, err)
			return created[0].ID
		}
		popular, quiet := tag("popular"), tag("quiet")
		seed := func(id entities.EntityID) string {
			created, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{
				{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Ranked " + string(id)}}}}})
			must(t, err)
			exec(`UPDATE `+table("content_facets")+` SET live_from=now()-interval '1 hour',entity_ids=$3::text[] WHERE tenant_id=$1 AND content_id=$2`,
				app, created.ID, []string{string(id)})
			return created.ID
		}
		ranked := map[string]int{seed(popular): 30, seed(popular): 30, seed(quiet): 5, seed(quiet): 5}
		awaitPublicationJobsIdle(t, w, a)
		a.server.Close()
		must(t, a.engine.Close(ctx))

		// The previous release's table: built by its own migrations in a
		// scratch schema, filled with these rows in its text form, and the
		// conversion unrecorded.
		steps, err := migratekit.Load(migrations.Postgres, ".")
		must(t, err)
		first := slices.IndexFunc(steps, func(m migratekit.Migration) bool { return strings.Contains(m.Name, "_favorites_binary_keys.") })
		if first < 0 || !strings.Contains(steps[first+1].Name, "_favorites_copy.") || !strings.Contains(steps[first+2].Name, "_favorites_swap.") {
			t.Fatal("the favorites conversion is not three consecutive migrations")
		}
		db := stdlib.OpenDBFromPool(w.pool)
		t.Cleanup(func() { _ = db.Close() })
		previous := w.schema + "_previous"
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{previous}.Sanitize()+" CASCADE")
			_, _ = w.pool.Exec(ctx, "DELETE FROM public.migrations WHERE app='contentkit' AND schema=$1", previous)
		})
		apply := func(schema string, through int) {
			t.Helper()
			must(t, migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps[:through+1]))
		}
		apply(previous, first-1)
		sequence, err := migratekit.Sequence(steps[first].Name)
		must(t, err)
		exec(`CREATE TABLE ` + table("critical_converted") + ` AS TABLE ` + table("content_favorites"))
		exec(`DROP TABLE ` + table("content_favorites"))
		exec(`CREATE TABLE ` + table("content_favorites") + ` (LIKE ` + pgx.Identifier{previous, "content_favorites"}.Sanitize() + ` INCLUDING ALL)`)
		exec(`INSERT INTO ` + table("content_favorites") + ` (tenant_id, user_id, content_kind, content_id, content_version_id, value, revision,
 created_at, updated_at, anonymous_actor_id, import_source)
 SELECT app_id::text, user_id::text, content_kind, content_id::text, content_version_id::text, value, revision,
 created_at, updated_at, anonymous_actor_id, import_source FROM ` + table("critical_converted"))
		exec(`DROP TABLE ` + table("critical_converted"))
		exec(`DELETE FROM public.migrations WHERE app='contentkit' AND schema=$1 AND sequence >= $2`, w.schema, sequence)
		account := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
		for id, accounts := range ranked {
			exec(`INSERT INTO `+table("content_favorites")+` (tenant_id, content_kind, content_id, user_id, value, revision, created_at)
 SELECT $1, 'article', $2, '00000000-0000-4000-8000-' || lpad(n::text, 12, '0'), 1, 1, now() - interval '1 day' FROM generate_series(1, $3::int) n`,
				app, id, accounts)
		}
		revision := `nextval('` + table("content_preference_revision_seq") + `')`
		u6, u7 := account(1006), account(1007)

		// Between the steps the previous release keeps writing; each kind of
		// write lands on a key the copy must settle.
		apply(w.schema, first)
		exec(`UPDATE `+table("content_favorites")+` SET value=0, revision=`+revision+`, updated_at=now()
 WHERE tenant_id=$1 AND user_id=$2 AND content_id=$3 AND content_version_id IS NULL`, app, u2.ID, item.ID)
		exec(`UPDATE `+table("content_favorites")+` SET user_id=NULL, anonymous_actor_id=gen_random_uuid() WHERE tenant_id=$1 AND user_id=$2`, app, u5.ID)
		exec(`DELETE FROM `+table("content_favorites")+` WHERE tenant_id=$1 AND user_id=$2`, app, account(1))
		exec(`INSERT INTO `+table("content_favorites")+` (tenant_id, user_id, content_kind, content_id, value, revision, import_source)
 VALUES ($1, $2, 'article', $3, 1, `+revision+`, '')`, app, u6, item.ID)
		apply(w.schema, first+1)
		exec(`UPDATE `+table("content_favorites")+` SET value=0, revision=`+revision+`, updated_at=now() WHERE tenant_id=$1 AND user_id=$2`, app, u6)
		exec(`INSERT INTO `+table("content_favorites")+` (tenant_id, user_id, content_kind, content_id, value, revision, import_source)
 VALUES ($1, $2, 'article', $3, 1, `+revision+`, '')`, app, u7, item.ID)
		rows := func(appColumn string) []string {
			t.Helper()
			r, err := w.pool.Query(ctx, `SELECT format('%L|%L|%L|%L|%L|%L|%L|%L|%L|%L|%L', `+appColumn+`, user_id, anonymous_actor_id, content_kind, content_id,
 content_version_id, value, revision, created_at, updated_at, import_source) AS row FROM `+table("content_favorites")+` ORDER BY row`)
			must(t, err)
			out, err := pgx.CollectRows(r, pgx.RowTo[string])
			must(t, err)
			return out
		}
		want := rows("tenant_id")

		// The engine's startup migration finishes the conversion.
		configureDiscoverySignals(t, w, a)
		a.engine, err = ck.New(ctx, a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		if got := rows("app_id"); !reflect.DeepEqual(got, want) {
			t.Fatalf("conversion changed favorites:\n got %q\nwant %q", got, want)
		}
		var shape string
		must(t, w.pool.QueryRow(ctx, `SELECT string_agg(attname || ' ' || format_type(atttypid, atttypmod), ', ' ORDER BY attname)
 FROM pg_attribute WHERE attrelid = $1::regclass AND attname IN ('app_id', 'tenant_id', 'user_id', 'content_id', 'content_version_id') AND NOT attisdropped`,
			table("content_favorites")).Scan(&shape))
		if shape != "app_id uuid, content_id uuid, content_version_id uuid, user_id uuid" {
			t.Fatalf("converted keys are %s", shape)
		}
		var indexes []string
		r, err := w.pool.Query(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname=$1 AND tablename='content_favorites' ORDER BY 1`, w.schema)
		must(t, err)
		indexes, err = pgx.CollectRows(r, pgx.RowTo[string])
		must(t, err)
		if !reflect.DeepEqual(indexes, []string{"content_favorites_content_idx", "content_favorites_popular_idx", "content_favorites_revision_idx",
			"content_favorites_user_created_idx", "content_favorites_user_key"}) {
			t.Fatalf("converted indexes are %v", indexes)
		}
		var leftovers int
		must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname=$1 AND c.relname IN ('content_favorites_next', 'content_favorites_moved')`, w.schema).Scan(&leftovers))
		if leftovers != 0 {
			t.Fatal("the conversion left its working tables")
		}
		must(t, a.engine.StartJobs(ctx))

		// Recount: a new favorite recounts the item from every converted row.
		result, err := admin.PostImport(ctx, importer.Batch{Source: "after-conversion", Favorites: []importer.FavoriteRecord{
			{Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}}, UserID: account(1008)}}})
		must(t, err)
		if len(result.Failed) != 0 {
			t.Fatalf("import after the conversion failed: %+v", result.Failed)
		}
		// u1, u4 and u5 anonymized, u7 and the import.
		state, err := a.client(token1).GetContentFavorite(ctx, item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !state.Favorited || state.Count == nil || *state.Count != 5 {
			t.Fatalf("recount over converted rows: %+v; want favorited by 5", state)
		}

		// Toggle: the converted row is the one toggled, never a second.
		for _, on := range []bool{true, false, true} {
			if on {
				state, err = a.client(token1).PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{})
			} else {
				state, err = a.client(token1).DeleteContentFavorite(ctx, item.ID, ck.DeleteContentFavoriteQuery{})
			}
			must(t, err)
			if n := map[bool]int{true: 5, false: 4}[on]; state.Favorited != on || state.Count == nil || *state.Count != n {
				t.Fatalf("toggle to %v: %+v; want count %d", on, state, n)
			}
		}
		var mine int
		must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+table("content_favorites")+` WHERE app_id=$1 AND user_id=$2 AND content_id=$3`,
			app, u1.ID, item.ID).Scan(&mine))
		if mine != 2 {
			t.Fatalf("u1 holds %d favorite rows of the item; want its work and version rows", mine)
		}

		// Card state and the reader's own list.
		for token, favorited := range map[string]bool{token1: true, token2: false} {
			page, err := a.client(token).GetContents(ctx, ck.GetContentsQuery{Ids: []string{item.ID}})
			must(t, err)
			if len(page.Contents) != 1 || page.Contents[0].Viewer == nil || page.Contents[0].Viewer.Favorited != favorited {
				t.Fatalf("card state over converted rows: %+v; want favorited %v", page.Contents, favorited)
			}
		}
		listed, err := a.client(token1).GetMeFavorites(ctx, ck.GetMeFavoritesQuery{})
		must(t, err)
		if len(listed.Items) != 1 || listed.Items[0].Content == nil || listed.Items[0].Content.ID != item.ID {
			t.Fatalf("the reader's favorites over converted rows: %+v", listed.Items)
		}

		// Preference sync sends every account's converted favorite.
		queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
		must(t, err)
		runJob := func(args river.JobArgs) {
			t.Helper()
			job, err := queue.Insert(ctx, args, &river.InsertOpts{Queue: "contentkit", MaxAttempts: 1})
			must(t, err)
			waitForWithin(t, time.Minute, func() bool {
				row, err := queue.JobGet(ctx, job.Job.ID)
				must(t, err)
				if row.State == rivertype.JobStateDiscarded {
					t.Fatalf("%s failed: %+v", args.Kind(), row.Errors)
				}
				return row.State == rivertype.JobStateCompleted
			})
		}
		runJob(preferenceResync{Resync: true})
		preferences := func(query string, run func(string, ...any) ([]string, error)) []string {
			t.Helper()
			out, err := run(query, app)
			must(t, err)
			slices.Sort(out)
			return out
		}
		sent := preferences(`SELECT concat(subject, '|', content_id, '|', content_version_id, '|', toString(toInt64(value))) FROM `+a.cfg.ClickHouse.Database+`.signals FINAL
 WHERE tenant = ? AND signal_type = '`+content.PreferenceAxisFavorite+`'`, func(q string, args ...any) ([]string, error) {
			r, err := a.deps.ClickHouse.Query(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			var out []string
			for r.Next() {
				var s string
				if err := r.Scan(&s); err != nil {
					return nil, err
				}
				out = append(out, s)
			}
			return out, r.Err()
		})
		stored := preferences(`SELECT concat(user_id, '|', content_id, '|', coalesce(content_version_id::text, ''), '|', value)
 FROM `+table("content_favorites")+` WHERE app_id = $1 AND user_id IS NOT NULL`, func(q string, args ...any) ([]string, error) {
			r, err := w.pool.Query(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(r, pgx.RowTo[string])
		})
		// 66 ranking favorites and u1's two, u2's, u3's, u6's, u7's and the import.
		if len(stored) != 73 || !reflect.DeepEqual(sent, stored) {
			t.Fatalf("preference sync sent %d favorites; the converted table holds %d, want 73", len(sent), len(stored))
		}

		// Rankings: the converted favorites rank the popular tag alone.
		runJob(scheduledContentJob{kind: "contentkit_entity_popular_refresh"})
		r, err = w.pool.Query(ctx, `SELECT entity_id FROM `+table("content_entity_popular_rankings")+` WHERE tenant_id=$1 AND entity_kind='tag' ORDER BY place`, app)
		must(t, err)
		places, err := pgx.CollectRows(r, pgx.RowTo[entities.EntityID])
		must(t, err)
		if !slices.Equal(places, []entities.EntityID{popular}) {
			t.Fatalf("rankings over converted favorites are %v; want only %s", places, popular)
		}
	})
}
