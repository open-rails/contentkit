package critical_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
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
