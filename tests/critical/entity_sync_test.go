package critical_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
	"github.com/riverqueue/river"
)

func TestEntityLifecycleWithoutReconciliation(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["collection"] = kinds.EntityKind{Owner: "user"}
	})
	a := w.apps[0]
	tenant := a.cfg.Scope.AppID.String()
	jobs := pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()
	entityTable := pgx.Identifier{w.schema, "content_entities"}.Sanitize()
	countJobs := func(id string) int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+jobs+" WHERE kind='contentkit_entity_sync' AND args->>'tenant'=$1 AND args->'groups' ? $2", tenant, id).Scan(&n))
		return n
	}
	var imported entities.EntityDetail
	var collection entities.Entity
	var ownedContent string
	t.Run("CK-J16-S01_transactional_admission_without_bound_workers", func(t *testing.T) {
		owner, _, created := ownedEntity(t, w, a, "Immediate channel")
		if countJobs(string(created.ID)) != 1 {
			t.Fatal("unbound ordinary creation did not persist lifecycle work")
		}
		store, err := entities.New(entities.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: a.cfg.Content, Scopes: a.deps.Scopes})
		must(t, err)
		tx, err := w.pool.Begin(t.Context())
		must(t, err)
		draft, err := store.CreateEntitiesTx(t.Context(), tx, []entities.EntityInput{{Kind: "channel", Slug: "rolled-back", OwnerUserID: owner.ID, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Rolled back"}}}})
		must(t, err)
		var inTx int
		must(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM "+jobs+" WHERE kind='contentkit_entity_sync' AND args->'groups' ? $1", string(draft[0].ID)).Scan(&inTx))
		if inTx != 1 {
			t.Fatal("entity write did not atomically admit its lifecycle job")
		}
		must(t, tx.Rollback(t.Context()))
		if countJobs(string(draft[0].ID)) != 0 {
			t.Fatal("rolled-back entity left a committed job")
		}
		var exists bool
		must(t, w.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM "+entityTable+" WHERE tenant_id=$1 AND id=$2)", tenant, draft[0].ID).Scan(&exists))
		if exists {
			t.Fatal("rolled-back entity survived")
		}

		id := uuid.Must(uuid.NewV7()).String()
		result, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "offline-lifecycle", ProvisionScopes: true, Entities: []importer.EntityRecord{{ExternalID: "channel", ID: id, Kind: "channel", Slug: "offline-channel", OwnerUserID: owner.ID, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Imported channel"}}}}})
		must(t, err)
		if len(result.Failed) != 0 {
			t.Fatalf("import failed: %+v", result.Failed)
		}
		imported, err = a.engine.Client().GetManageEntity(t.Context(), id, ck.GetManageEntityQuery{})
		must(t, err)
		if imported.AuthScope != "" || countJobs(id) != 1 {
			t.Fatal("offline import did not leave exactly one durable unprocessed scope job")
		}
	})
	// S01 proves admission before any fleet is bound. Publication can now run,
	// while entity synchronization stays paused until S05 explicitly resumes it.
	fleet := publicationOnlyFleet(t, a)
	t.Run("CK-J16-S04_owned_import_state_changes_admit_exact_work", func(t *testing.T) {
		owner, token := w.person(t, 0)
		created, err := a.client(token).PostEntities(t.Context(), []entities.EntityInput{{Kind: "collection", Slug: "owned-collection", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Collection without permission group"}}}})
		must(t, err)
		collection = created[0]
		item, err := a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", OwnerEntityID: string(collection.ID), Text: map[string]catalog.Text{"en": {Title: "Collection post"}}})
		must(t, err)
		ownedContent = item.ID
		_, err = a.client(token).PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, a.client(token), item.ID, item.Versions[0].ID)
		result, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "offline-lifecycle", Entities: []importer.EntityRecord{{ExternalID: "collection", ID: string(collection.ID), Kind: "collection", Slug: collection.Slug, OwnerUserID: owner.ID, State: "deleted"}}})
		must(t, err)
		if len(result.Failed) != 0 {
			t.Fatalf("delete import failed: %+v", result.Failed)
		}
		if countJobs(string(collection.ID)) != 1 {
			t.Fatal("user-owned import deletion without auth_scope lost its job")
		}
		_, err = a.client(token).PostContents(t.Context(), catalog.ContentInput{Kind: "article", OwnerEntityID: string(collection.ID), Text: map[string]catalog.Text{"en": {Title: "Deleted owner denied"}}})
		if err == nil {
			t.Fatal("deleted entity retained authoring authority before worker ran")
		}
	})
	t.Run("CK-J16-S05_exact_jobs_converge_when_workers_resume", func(t *testing.T) {
		must(t, fleet.Queues().Add(entities.DefaultQueue, river.QueueConfig{MaxWorkers: 1}))
		waitFor(t, func() bool {
			current, err := a.engine.Client().GetManageEntity(t.Context(), string(imported.ID), ck.GetManageEntityQuery{})
			must(t, err)
			var state string
			must(t, w.pool.QueryRow(t.Context(), "SELECT state FROM "+pgx.Identifier{w.schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", tenant, ownedContent).Scan(&state))
			return current.AuthScope != "" && state == "deleted"
		})
		groupID, err := ckauth.EntityScopeID(*a.cfg.Scope, string(imported.ID))
		must(t, err)
		group, err := w.auth.Group(t.Context(), iam.GroupByID(groupID))
		must(t, err)
		if group.DeletedAt != nil {
			t.Fatal("queued scope was not provisioned")
		}
		var reconciles int
		must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+jobs+" WHERE kind='contentkit_entity_reconcile'").Scan(&reconciles))
		if reconciles != 0 {
			t.Fatal("worker resume scheduled the removed reconciliation poller")
		}
		result, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "offline-lifecycle", Entities: []importer.EntityRecord{{ExternalID: "collection", ID: string(collection.ID), Kind: "collection", Slug: collection.Slug, OwnerUserID: collection.OwnerUserID, State: "active"}}})
		must(t, err)
		if len(result.Failed) != 0 {
			t.Fatalf("restore import failed: %+v", result.Failed)
		}
		waitFor(t, func() bool {
			var state string
			must(t, w.pool.QueryRow(t.Context(), "SELECT state FROM "+pgx.Identifier{w.schema, "content_items"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", tenant, ownedContent).Scan(&state))
			return state == "published"
		})
	})
}
