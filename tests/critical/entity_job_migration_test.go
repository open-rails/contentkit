package critical_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type oldEntityJob struct {
	Tenant string   `json:"tenant,omitempty"`
	Groups []string `json:"groups,omitempty"`
	kind   string
}

func (a oldEntityJob) Kind() string { return a.kind }

func TestEntityJobMigration(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	ctx := t.Context()
	tenant := a.cfg.Scope.AppID.String()
	jobs := pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()
	client, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	insert := func(kind, tenant, queue string, ids ...string) int64 {
		t.Helper()
		job, err := client.Insert(ctx, oldEntityJob{Tenant: tenant, Groups: ids, kind: kind}, &river.InsertOpts{Queue: queue})
		must(t, err)
		return job.Job.ID
	}
	var scopeID string
	t.Run("CK-J16-S01_migration_recovers_only_exact_unfinished_lifecycles", func(t *testing.T) {
		owner, _ := w.person(t, 0)
		scopeID = uuid.Must(uuid.NewV7()).String()
		result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: "migration-gap", ProvisionScopes: true, Entities: []importer.EntityRecord{{ExternalID: "gap", ID: scopeID, Kind: "channel", Slug: "migration-gap", OwnerUserID: owner.ID}}})
		must(t, err)
		if len(result.Failed) != 0 {
			t.Fatalf("fixture import: %+v", result.Failed)
		}
		// Reproduce an old accepted import that had no bound producer and no job.
		_, err = w.pool.Exec(ctx, "DELETE FROM "+jobs+" WHERE kind='contentkit_entity_sync' AND args->>'tenant'=$1 AND args->'groups' ? $2", tenant, scopeID)
		must(t, err)
		own := insert("contentkit_entity_reconcile", tenant, entities.DefaultQueue)
		foreign := insert("contentkit_entity_reconcile", w.apps[1].cfg.Scope.AppID.String(), entities.DefaultQueue)
		foreignQueue := insert("contentkit_entity_reconcile", tenant, "another-app-queue")
		projection := insert("contentkit_entity_projection", tenant, "contentkit")
		oldSync := insert("contentkit_group_sync", tenant, "contentkit_groups", scopeID)
		terminal := insert("contentkit_entity_projection", "", "contentkit")
		_, err = w.pool.Exec(ctx, "UPDATE "+jobs+" SET state='completed',finalized_at=clock_timestamp() WHERE id=$1", terminal)
		must(t, err)
		must(t, ck.MigratePostgres(ctx, w.pool, a.cfg))
		must(t, ck.MigratePostgres(ctx, w.pool, a.cfg))
		for id, state := range map[int64]string{own: "cancelled", foreign: "available", foreignQueue: "available", projection: "cancelled", terminal: "completed", oldSync: "available"} {
			var actual string
			must(t, w.pool.QueryRow(ctx, "SELECT state FROM "+jobs+" WHERE id=$1", id).Scan(&actual))
			if actual != state {
				t.Fatalf("job %d state=%s want%s", id, actual, state)
			}
		}
		var kind, queue string
		must(t, w.pool.QueryRow(ctx, "SELECT kind,queue FROM "+jobs+" WHERE id=$1", oldSync).Scan(&kind, &queue))
		if kind != "contentkit_entity_sync" || queue != entities.DefaultQueue {
			t.Fatalf("exact sync was not retained: %s/%s", kind, queue)
		}
		var repairs int
		must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM "+jobs+" WHERE kind='contentkit_entity_sync' AND args->>'tenant'=$1 AND args->'groups' ? $2 AND id<>$3", tenant, scopeID, oldSync).Scan(&repairs))
		if repairs != 1 {
			t.Fatalf("repeated migration admitted %d repair jobs; want1", repairs)
		}
	})
	t.Run("CK-J16-S03_migration_refuses_running_or_ambiguous_entity_work", func(t *testing.T) {
		id := insert("contentkit_entity_reconcile", tenant, entities.DefaultQueue)
		_, err := w.pool.Exec(ctx, "UPDATE "+jobs+" SET state='running',attempted_at=clock_timestamp(),attempt=1 WHERE id=$1", id)
		must(t, err)
		err = ck.MigratePostgres(ctx, w.pool, a.cfg)
		if err == nil || !strings.Contains(err.Error(), "stop old entity workers") {
			t.Fatalf("running work was not refused: %v", err)
		}
		_, err = w.pool.Exec(ctx, "UPDATE "+jobs+" SET state='available' WHERE id=$1", id)
		must(t, err)
		unknown := insert("contentkit_entity_projection", "", "contentkit")
		err = ck.MigratePostgres(ctx, w.pool, a.cfg)
		if err == nil || !strings.Contains(err.Error(), "ownership is ambiguous") {
			t.Fatalf("tenantless work was not refused: %v", err)
		}
		var state string
		must(t, w.pool.QueryRow(ctx, "SELECT state FROM "+jobs+" WHERE id=$1", unknown).Scan(&state))
		if state != "available" {
			t.Fatal("ambiguous work changed")
		}
	})
}
