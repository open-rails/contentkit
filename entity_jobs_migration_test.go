package contentkit

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

type retiredWorkerArgs string

func (a retiredWorkerArgs) Kind() string { return string(a) }

func assertRetiredWorkersAbsent(t *testing.T, e *Engine) {
	t.Helper()
	client := e.jobs.bound()
	if client == nil {
		t.Fatal("root fleet is not bound")
	}
	for _, kind := range []string{"contentkit_media_expose", "contentkit_media_ready", "contentkit_media_recover", "contentkit_media_cleanup_due", "contentkit_media_delete_folder", "contentkit_groups", "contentkit_group_sync", "contentkit_group_reconcile"} {
		_, err := client.Insert(t.Context(), retiredWorkerArgs(kind), nil)
		var unknown *river.UnknownJobKindError
		if !errors.As(err, &unknown) {
			t.Fatalf("root fleet still accepts removed worker %s: %v", kind, err)
		}
		if client.PeriodicJobs().RemoveByID(kind) {
			t.Fatalf("root fleet still schedules %s", kind)
		}
	}
}

func TestEntityJobMigrationKeepsOwnershipAndHistory(t *testing.T) {
	e := purgeEventEngine(t, false)
	ctx := t.Context()
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	insert := func(kind, queue, state, tenant string) int64 {
		t.Helper()
		var id int64
		var finalized *time.Time
		if state == "completed" {
			now := time.Now()
			finalized = &now
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO `+jobs+`(kind,args,queue,state,max_attempts,finalized_at) VALUES($1,jsonb_build_object('tenant',$2::text,'groups',jsonb_build_array('kept')), $3,$4,3,$5) RETURNING id`, kind, tenant, queue, state, finalized).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	noop := insert("contentkit_groups", QueueContentKit, "available", "app")
	old := insert("contentkit_group_sync", "contentkit_groups", "running", "app")
	foreign := insert("contentkit_group_sync", "contentkit_groups", "available", "other")
	terminal := insert("contentkit_groups", QueueContentKit, "completed", "app")
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.migrateEntityJobsTx(ctx, tx, jobs) }); err == nil {
		t.Fatal("running old worker accepted")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE `+jobs+` SET state='available' WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.migrateCatalogEvents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id int64) (string, string, string) {
		t.Helper()
		var kind, queue, state string
		if err := e.pool.QueryRow(ctx, `SELECT kind,queue,state FROM `+jobs+` WHERE id=$1`, id).Scan(&kind, &queue, &state); err != nil {
			t.Fatal(err)
		}
		return kind, queue, state
	}
	if _, _, state := read(noop); state != "cancelled" {
		t.Fatalf("obsolete no-op state=%s", state)
	}
	if kind, queue, state := read(old); kind != "contentkit_entity_sync" || queue != "contentkit_entities" || state != "available" {
		t.Fatalf("real work not transferred: %s %s %s", kind, queue, state)
	}
	if kind, queue, state := read(foreign); kind != "contentkit_group_sync" || queue != "contentkit_groups" || state != "available" {
		t.Fatal("foreign work changed")
	}
	if kind, _, state := read(terminal); kind != "contentkit_groups" || state != "completed" {
		t.Fatal("terminal history changed")
	}
	if err := e.validateCatalogEventHandoff(ctx); err != nil {
		t.Fatal(err)
	}
}
