package critical_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/internal/pglock"
)

type poolerProbeArgs struct{}

func (poolerProbeArgs) Kind() string { return "critical_pooler_probe" }

type poolerProbe struct {
	river.WorkerDefaults[poolerProbeArgs]
	ran chan<- time.Time
}

func (w *poolerProbe) Work(context.Context, *river.Job[poolerProbeArgs]) error {
	w.ran <- time.Now()
	return nil
}

// ContentKit behind PgBouncer in transaction mode (max_prepared_statements=0)
// with Deps.PostgresSession straight to PostgreSQL.
func TestTransactionPooler(t *testing.T) {
	bouncer, direct := os.Getenv("CONTENTKIT_CRITICAL_PGBOUNCER"), os.Getenv("CONTENTKIT_CRITICAL_PG")
	if bouncer == "" || direct == "" {
		t.Fatal("CONTENTKIT_CRITICAL_PGBOUNCER and CONTENTKIT_CRITICAL_PG are required")
	}
	ctx := t.Context()
	w := newWorld(t)
	a := w.apps[0]
	name := func(prefix string) string { return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
	sessionName := name("ck-session-")
	session, err := pgxpool.ParseConfig(direct)
	must(t, err)
	session.ConnConfig.RuntimeParams["application_name"] = sessionName
	pooled := func(t *testing.T, mode pgx.QueryExecMode) *pgxpool.Pool {
		cfg, err := pgxpool.ParseConfig(bouncer)
		must(t, err)
		cfg.ConnConfig.DefaultQueryExecMode = mode
		cfg.MaxConns = 16 // more clients than PgBouncer's 8 server connections
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		must(t, err)
		t.Cleanup(pool.Close)
		return pool
	}

	t.Run("CK-J01-S18_named_statements_break_behind_the_pooler_and_are_refused", func(t *testing.T) {
		// Two clients of the one-connection alias share its server connection,
		// so the second client's named statement collides with the first's.
		one := func(mode pgx.QueryExecMode) *pgx.Conn {
			cfg, err := pgx.ParseConfig(bouncer)
			must(t, err)
			cfg.Database, cfg.DefaultQueryExecMode = "critical_one", mode
			conn, err := pgx.ConnectConfig(ctx, cfg)
			must(t, err)
			t.Cleanup(func() { _ = conn.Close(context.Background()) })
			return conn
		}
		const sql = "SELECT pg_backend_pid()"
		var first, second int32
		must(t, one(pgx.QueryExecModeCacheStatement).QueryRow(ctx, sql).Scan(&first))
		var pe *pgconn.PgError
		if err := one(pgx.QueryExecModeCacheStatement).QueryRow(ctx, sql).Scan(&second); !errors.As(err, &pe) || pe.Code != "42P05" {
			t.Fatalf("a second client's named statement on the shared server connection: %v", err)
		}
		for range 2 {
			must(t, one(pgx.QueryExecModeCacheDescribe).QueryRow(ctx, sql).Scan(&second))
			if second != first {
				t.Fatalf("cache_describe clients ran on backend %d, not the alias's one %d", second, first)
			}
		}
		deps := a.deps
		deps.Postgres, deps.PostgresSession = pooled(t, pgx.QueryExecModeCacheStatement), session
		if _, err := ck.New(ctx, a.cfg, deps); err == nil || !strings.Contains(err.Error(), "cache_describe") {
			t.Fatalf("a pooled Postgres with named statements was accepted: %v", err)
		}
	})

	// New migrates through the session: the migrators' session locks would
	// otherwise be taken and released on different server connections.
	deps := a.deps
	deps.Postgres, deps.PostgresSession = pooled(t, pgx.QueryExecModeCacheDescribe), session
	engine, err := ck.New(ctx, a.cfg, deps)
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.Close(ctx); err != nil {
			t.Errorf("close pooled engine: %v", err)
		}
	})
	ran := make(chan time.Time, 4)
	probe := riverhelpers.NewContribution("critical-pooler", func(_ context.Context, cfg *river.Config) error {
		cfg.Queues["critical_pooler"] = river.QueueConfig{MaxWorkers: 1}
		return river.AddWorkerSafely(cfg.Workers, &poolerProbe{ran: ran})
	}, nil, nil)
	// A 30-second poll: a job that starts sooner was woken by its notification.
	fleet, err := jobhook.Fleet(ctx, engine, &river.Config{Schema: w.schema + "_river", FetchPollInterval: 30 * time.Second,
		Queues: map[string]river.QueueConfig{}}, probe)
	must(t, err)
	must(t, fleet.Start(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	directPool := w.pool

	t.Run("CK-J01-S18_jobs_work_through_the_pooler_and_hear_their_notifications", func(t *testing.T) {
		run := func() time.Duration {
			at := time.Now()
			_, err := fleet.Insert(ctx, poolerProbeArgs{}, &river.InsertOpts{Queue: "critical_pooler"})
			must(t, err)
			select {
			case started := <-ran:
				return started.Sub(at)
			case <-time.After(45 * time.Second):
				t.Fatal("the probe job never ran")
				return 0
			}
		}
		run() // River fetches once at start; by then its listener is connecting
		time.Sleep(2 * time.Second)
		for range 3 {
			if waited := run(); waited > 5*time.Second {
				t.Fatalf("a job waited %s for the 30 s poll: its notification never reached the listener", waited)
			}
		}
		var listeners int
		must(t, directPool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, sessionName).Scan(&listeners))
		if listeners == 0 {
			t.Fatal("River listens on no direct session")
		}
	})

	t.Run("CK-J01-S18_source_locks_hold_a_direct_session_and_exclude_another_process", func(t *testing.T) {
		admin := engine.Client()
		item, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"en"},
			Text: map[string]catalog.VersionText{"en": {Title: "Pooled source lock"}}}}})
		must(t, err)
		version := item.Versions[0].ID
		other := session.Copy()
		other.ConnConfig.RuntimeParams["application_name"] = name("ck-holder-")
		release, ok, err := pglock.New(other).Acquire(ctx, "contentkit:media:contentkit:versions:"+a.cfg.Scope.AppID.String()+":"+version, false)
		must(t, err)
		if !ok {
			t.Fatal("the other process could not take a free source lock")
		}
		defer release()
		written := make(chan error, 1)
		go func() {
			_, err := admin.PutContentVersionSegments(ctx, item.ID, version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{}})
			written <- err
		}()
		// The write queues for the key on a direct session of its own.
		waitFor(t, func() bool {
			var waiting int
			must(t, directPool.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity s ON s.pid=l.pid
 WHERE l.locktype='advisory' AND NOT l.granted AND s.application_name=$1`, sessionName).Scan(&waiting))
			return waiting == 1
		})
		select {
		case err := <-written:
			t.Fatalf("segments were written while another process held the source lock: %v", err)
		default:
		}
		release()
		select {
		case err := <-written:
			must(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the write never took the released source lock")
		}
	})

	t.Run("CK-J01-S18_concurrent_writes_through_the_pooler_prepare_no_statement", func(t *testing.T) {
		admin := engine.Client()
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 16 {
			wg.Go(func() {
				title := fmt.Sprintf("Pooled write %d", i)
				item, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"},
					Text: map[string]catalog.VersionText{"en": {Title: title, Body: "Through PgBouncer"}}}}})
				if err == nil && item.ID == "" {
					err = fmt.Errorf("write %d answered no content", i)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		// The engine's own start-up jobs ran through the pooler too.
		var failed int
		must(t, directPool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+
			` WHERE errors::text ILIKE '%prepared statement%'`).Scan(&failed))
		if failed != 0 {
			t.Fatalf("%d jobs failed on a prepared statement behind the pooler", failed)
		}
	})
}
