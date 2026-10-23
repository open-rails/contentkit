package contentkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
)

func purgeEventEngine(t *testing.T, signals bool) *Engine {
	t.Helper()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	riverSchema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	e := &Engine{pool: pool, schema: schema, riverSchema: riverSchema, cfg: Config{Tenant: "app"}}
	if signals {
		e.cfg.ClickHouse.Database = "configured-but-currently-down"
	}
	return e
}

func TestCatalogPurgeRecordsDurableWorkWithoutRunningFleet(t *testing.T) {
	e := purgeEventEngine(t, true)
	ctx := t.Context()
	count := func() int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()+` WHERE kind='contentkit_forget_contents'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	refs := []contentref.ContentRef{contentref.New("app", "gallery", "parent").WithVersion("edition")}
	for _, commit := range []bool{false, true} {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.forgetPurgedTx(ctx, tx, refs); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if commit {
			want = 1
		}
		if got := count(); got != want {
			t.Fatalf("commit=%v queued=%d want=%d", commit, got, want)
		}
	}
}

func TestCatalogPurgeHandoffIsAppScopedAndAtomic(t *testing.T) {
	e := purgeEventEngine(t, true)
	ctx := t.Context()
	table := pgx.Identifier{e.schema, "content_event_purge_handoff"}.Sanitize()
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	if _, err := e.pool.Exec(ctx, `INSERT INTO `+table+`(tenant_id,event_id,content_kind,content_id,content_version_id) VALUES ('app',1,'gallery','one',NULL),('app',2,'gallery','two','edition'),('other-app',3,'gallery','keep',NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := e.validateCatalogEventHandoff(ctx); err == nil {
		t.Fatal("startup accepted an unfinished handoff")
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE `+jobs+` ADD CONSTRAINT reject_forget CHECK(kind <> 'contentkit_forget_contents')`); err != nil {
		t.Fatal(err)
	}
	if err := e.migrateCatalogEvents(ctx); err == nil {
		t.Fatal("failed queue insert lost no error")
	}
	var retained int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("failed transfer removed facts: %d %v", retained, err)
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE `+jobs+` DROP CONSTRAINT reject_forget`); err != nil {
		t.Fatal(err)
	}
	if err := e.migrateCatalogEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.migrateCatalogEvents(ctx); err != nil {
		t.Fatalf("repeat transfer: %v", err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id='other-app'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("another app's obligation removed: %d %v", retained, err)
	}
	var raw []byte
	if err := e.pool.QueryRow(ctx, `SELECT args FROM `+jobs+` WHERE kind='contentkit_forget_contents'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var args forgetContentsArgs
	if err := json.Unmarshal(raw, &args); err != nil || len(args.Contents) != 2 || args.Contents[1].VersionID != "edition" {
		t.Fatalf("incomplete purge facts: %s %v", raw, err)
	}
	if err := e.validateCatalogEventHandoff(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPurgeHandoffSurvivesMissingMigrationSignalConfiguration(t *testing.T) {
	e := purgeEventEngine(t, false)
	ctx := t.Context()
	if _, err := e.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{e.schema, "content_event_purge_handoff"}.Sanitize()+`(tenant_id,event_id,content_kind,content_id) VALUES('app',1,'gallery','one')`); err != nil {
		t.Fatal(err)
	}
	if err := e.migrateCatalogEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.validateCatalogEventHandoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()+` WHERE kind='contentkit_forget_contents'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("legacy obligation lost without migration Deps: %d %v", jobs, err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := e.forgetPurgedTx(ctx, tx, []contentref.ContentRef{contentref.New("app", "gallery", "new")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()+` WHERE kind='contentkit_forget_contents'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("new unconfigured purge unexpectedly queued work: %d %v", jobs, err)
	}
}

func TestCatalogEventPreflightRefusesRunningOldWorkerBeforeDDL(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	riverSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE `+pgx.Identifier{schema, "content_events"}.Sanitize()+`(delivered_at timestamptz); INSERT INTO `+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+` (kind,args,queue,max_attempts,state) VALUES ('contentkit_catalog_deliver_events','{}','contentkit_catalog',3,'running')`); err != nil {
		t.Fatal(err)
	}
	err := MigratePostgres(ctx, pool, Config{Tenant: "app", Database: DatabaseConfig{Schema: schema, RiverSchema: riverSchema}})
	if err == nil || !strings.Contains(err.Error(), "stop old catalog") {
		t.Fatalf("old worker crossed migration preflight: %v", err)
	}
	var preserved bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='content_events' AND column_name='delivered_at')`, schema).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("old schema changed before refusal: %v %v", preserved, err)
	}
}

func TestCatalogEventFenceLeavesNewInboxReaderUntouched(t *testing.T) {
	e := purgeEventEngine(t, false)
	ctx := t.Context()
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	if _, err := e.pool.Exec(ctx, `INSERT INTO `+jobs+`(kind,args,queue,max_attempts,state) VALUES ('contentkit_openrails_inbox_sync','{}','contentkit',3,'running'),('contentkit_openrails_notifications','{}','contentkit',3,'scheduled')`); err != nil {
		t.Fatal(err)
	}
	if err := e.preflightCatalogEventMigration(ctx); err != nil {
		t.Fatalf("new reader was mistaken for a legacy worker: %v", err)
	}
	if err := e.validateCatalogEventHandoff(ctx); err == nil {
		t.Fatal("pending old transport was not detected")
	}
	if err := e.migrateCatalogEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.validateCatalogEventHandoff(ctx); err != nil {
		t.Fatalf("new reader blocked startup: %v", err)
	}
	var state string
	if err := e.pool.QueryRow(ctx, `SELECT state FROM `+jobs+` WHERE kind='contentkit_openrails_inbox_sync'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("new reader was cancelled: %s %v", state, err)
	}
}
