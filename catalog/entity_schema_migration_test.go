package catalog

import (
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/helpers/eventlog"
	"github.com/open-rails/migratekit"
)

// Naming must not create replacement entities, billing products or event streams.
// Exercise a legacy retained-log database and the immediately preceding runtime
// database, preserving their actual cursor and persisted JSON encodings.
func TestEntitySchemaUpgradePreservesIdentityAndReplay(t *testing.T) {
	for _, start := range []string{"0032_catalog_event_log.up.sql", "0038_runtime_entity_reads.up.sql"} {
		t.Run(start, func(t *testing.T) {
			ctx := t.Context()
			pool := pgtest.Pool(t, nil)
			pgtest.EnsureExtensions(t, ctx, pool)
			schema := pgtest.EmptySchema(t, ctx, pool)
			q := pgx.Identifier{schema}.Sanitize()
			db := stdlib.OpenDBFromPool(pool)
			defer db.Close()
			steps, err := migratekit.Load(migrations.Postgres, ".")
			if err != nil {
				t.Fatal(err)
			}
			at := func(name string) int {
				t.Helper()
				n := slices.IndexFunc(steps, func(s migratekit.Migration) bool { return s.Name == name })
				if n < 0 {
					t.Fatalf("missing migration %s", name)
				}
				return n + 1
			}
			migrator := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
			if err := migrator.ApplyMigrations(ctx, steps[:at("0032_catalog_event_log.up.sql")]); err != nil {
				t.Fatal(err)
			}
			tenant := "rename"
			content, version, entity, offer := contentref.NewID(), contentref.NewID(), contentref.NewID(), contentref.NewID()
			key := "rename:group:" + entity
			policy := `{"level":"group","group_id":"` + entity + `"}`
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			// These are deliberately pre0039 table/column and serialized names.
			exec(`INSERT INTO `+q+`.content_groups(tenant_id,id,kind,slug,access,ever_sold) VALUES($1,$2,'tag','preserved',$3,true)`, tenant, entity, policy)
			exec(`INSERT INTO `+q+`.content_group_names(tenant_id,group_id,language,kind,name) VALUES($1,$2,'en','name','Preserved')`, tenant, entity)
			exec(`INSERT INTO `+q+`.content_items(tenant_id,id,kind,state,publish_at,published_at,access,owner_group_id) VALUES($1,$2,'post','published',now()-interval '1 day',now()-interval '1 day',$3,$4)`, tenant, content, policy, entity)
			exec(`INSERT INTO `+q+`.content_versions(tenant_id,id,content_id,languages,state) VALUES($1,$2,$3,'{en}','published')`, tenant, version, content)
			exec(`INSERT INTO `+q+`.content_memberships(tenant_id,group_id,content_id) VALUES($1,$2,$3)`, tenant, entity, content)
			exec(`INSERT INTO `+q+`.content_facets(tenant_id,content_id,version_id,kind,languages,live_from,access_level,access_group_id,group_ids,owner_group_id) VALUES($1,$2,$3,'post','{en}',now()-interval '1 day','group',$4,ARRAY[$4]::text[],$4)`, tenant, content, version, entity)
			exec(`INSERT INTO `+q+`.content_offers(tenant_id,id,target_kind,target_id,terms,product_id,price_id) VALUES($1,$2,'group',$3,'{"amount":99,"currency":"USD"}','archived-product','retained-price')`, tenant, offer, entity)
			operation := contentref.NewID()
			exec(`INSERT INTO `+q+`.content_publishing_operations(tenant_id,id,owner_key,request_hash,request,content_id,version_id,content_revision,version_revision,group_ids,commit_id)
 VALUES($1,$2,'publisher',repeat('a',64),'{"note":"pending before rename"}',$3,$4,1,1,ARRAY[$5]::text[],$6)`, tenant, operation, content, version, entity, contentref.NewID())
			// A value opaque to ContentKit stands in for the consumer-owned purchase key.
			payload := `{"kind":"post","content_id":"` + content + `","group_id":"` + entity + `","purchase_key":"` + key + `"}`
			exec(`INSERT INTO `+q+`.content_events(tenant_id,type,entity_id,payload,position) VALUES($1,'content.created',$2,$3,1),($1,'content.updated',$2,$3,2)`, tenant, content, payload)
			exec(`INSERT INTO `+q+`.content_event_streams(tenant_id,position) VALUES($1,2)`, tenant)
			var epoch, boundary, eventID string
			if err := pool.QueryRow(ctx, `SELECT epoch::text,(SELECT boundary::text FROM `+q+`.content_events WHERE tenant_id=$1 AND position=1),(SELECT id::text FROM `+q+`.content_events WHERE tenant_id=$1 AND position=2) FROM `+q+`.content_event_epoch`, tenant).Scan(&epoch, &boundary, &eventID); err != nil {
				t.Fatal(err)
			}
			cursor := encodeEventCursor(eventCursor{Version: 1, Scope: tenant, Epoch: epoch, Position: "1", Boundary: boundary, Types: []string{EventContentUpdated}})
			if err := migrator.ApplyMigrations(ctx, steps[:at(start)]); err != nil {
				t.Fatal(err)
			}
			snapshot := func(query string, args ...any) string {
				t.Helper()
				var s string
				if err := pool.QueryRow(ctx, query, args...).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			entityBefore := snapshot(`SELECT to_jsonb(e)::text FROM `+q+`.content_groups e WHERE tenant_id=$1 AND id=$2`, tenant, entity)
			offerBefore := snapshot(`SELECT (to_jsonb(o)-'content_id')::text FROM `+q+`.content_offers o WHERE tenant_id=$1 AND id=$2`, tenant, offer)
			operationBefore := snapshot(`SELECT (to_jsonb(o)-'group_ids')::text FROM `+q+`.content_publishing_operations o WHERE tenant_id=$1 AND id=$2`, tenant, operation)
			eventsBefore := snapshot(`SELECT jsonb_agg(to_jsonb(e) ORDER BY position)::text FROM `+q+`.content_events e WHERE tenant_id=$1`, tenant)
			for range 2 {
				if err := migrator.ApplyMigrations(ctx, steps[:at("0039_entity_terminology.up.sql")]); err != nil {
					t.Fatal(err)
				}
			}
			if got := snapshot(`SELECT to_jsonb(e)::text FROM `+q+`.content_entities e WHERE tenant_id=$1 AND id=$2`, tenant, entity); got != entityBefore {
				t.Fatalf("entity record changed: %s -> %s", entityBefore, got)
			}
			if got := snapshot(`SELECT (to_jsonb(o)-'content_id')::text FROM `+q+`.content_offers o WHERE tenant_id=$1 AND id=$2`, tenant, offer); got != offerBefore {
				t.Fatalf("billing record changed: %s -> %s", offerBefore, got)
			}
			if got := snapshot(`SELECT (to_jsonb(o)-'entity_ids')::text FROM `+q+`.content_publishing_operations o WHERE tenant_id=$1 AND id=$2`, tenant, operation); got != operationBefore {
				t.Fatalf("pending publishing receipt changed: %s -> %s", operationBefore, got)
			}
			var pendingEntities []string
			if err := pool.QueryRow(ctx, `SELECT entity_ids FROM `+q+`.content_publishing_operations WHERE tenant_id=$1 AND id=$2`, tenant, operation).Scan(&pendingEntities); err != nil || !slices.Equal(pendingEntities, []string{entity}) {
				t.Fatalf("pending publishing operation lost entity IDs: %v %v", pendingEntities, err)
			}
			if got := snapshot(`SELECT jsonb_agg(to_jsonb(e) ORDER BY position)::text FROM `+q+`.content_events e WHERE tenant_id=$1`, tenant); got != eventsBefore {
				t.Fatalf("retained facts changed: %s -> %s", eventsBefore, got)
			}
			var rule access.Rule
			if err := json.Unmarshal([]byte(snapshot(`SELECT access::text FROM `+q+`.content_items WHERE tenant_id=$1 AND id=$2`, tenant, content)), &rule); err != nil || rule.Entity != entity || rule.Level != access.EntityLevel {
				t.Fatalf("persisted policy lost: %+v %v", rule, err)
			}
			if got, err := access.EntityKey(tenant, entity); err != nil || got.String() != key {
				t.Fatalf("purchase key changed: %s %v", got, err)
			}
			store := &Store{pool: pool, schema: schema, qs: q, tenant: tenant, eventRetention: DefaultEventRetention, now: time.Now}
			page, err := store.EventReader().ListEvents(ctx, eventlog.Query{After: cursor, Types: []string{EventContentUpdated}})
			if err != nil || len(page.Events) != 1 || page.Events[0].ID != eventID {
				t.Fatalf("legacy cursor failed after rename: %+v %v", page, err)
			}
			continued, _, err := decodeEventCursor(page.NextCursor)
			if err != nil || continued.Epoch != epoch || continued.Position != "2" {
				t.Fatalf("stream restarted: %+v %v", continued, err)
			}
			var current, keywords []string
			if err := pool.QueryRow(ctx, `SELECT `+q+`.content_current_entity_ids($1,$2,$3),`+q+`.content_keyword_entity_ids($1,$2)`, tenant, content, version).Scan(&current, &keywords); err != nil || !slices.Equal(current, []string{entity}) || !slices.Equal(keywords, []string{entity}) {
				t.Fatalf("SQL functions lost renamed dependencies: current=%v keywords=%v %v", current, keywords, err)
			}
			var projected []string
			if err := pool.QueryRow(ctx, `SELECT entity_ids FROM `+q+`.content_current_facets WHERE tenant_id=$1 AND version_id=$2`, tenant, version).Scan(&projected); err != nil || !slices.Equal(projected, []string{entity}) {
				t.Fatalf("runtime view lost membership: %v %v", projected, err)
			}
			var absent bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, schema+".content_group_counts").Scan(&absent); err != nil || !absent {
				t.Fatalf("dead count table remains: %v %v", absent, err)
			}
			var fk, index, trigger string
			if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid=$1::regclass AND conname='content_entity_names_tenant_id_entity_id_fkey'`, schema+".content_entity_names").Scan(&fk); err != nil || !strings.Contains(fk, "entity_id") || !strings.Contains(fk, "content_entities") {
				t.Fatalf("FK binding changed: %s %v", fk, err)
			}
			if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass)`, schema+".content_memberships_entity_idx").Scan(&index); err != nil || !strings.Contains(index, "entity_id") {
				t.Fatalf("membership index binding changed: %s %v", index, err)
			}
			if err := pool.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid=$1::regclass AND tgname='content_entities_deleted'`, schema+".content_entities").Scan(&trigger); err != nil || !strings.Contains(trigger, "contentkit_entity_deleted") {
				t.Fatalf("trigger binding changed: %s %v", trigger, err)
			}
			exec(`UPDATE `+q+`.content_entities SET state='deleted' WHERE tenant_id=$1 AND id=$2`, tenant, entity)
			if got := snapshot(`SELECT (deleted_at IS NOT NULL)::text FROM `+q+`.content_entities WHERE tenant_id=$1 AND id=$2`, tenant, entity); got != "true" {
				t.Fatal("renamed deletion trigger did not run")
			}
			exec(`UPDATE `+q+`.content_entities SET state='active' WHERE tenant_id=$1 AND id=$2`, tenant, entity)
			if got := snapshot(`SELECT (deleted_at IS NULL)::text FROM `+q+`.content_entities WHERE tenant_id=$1 AND id=$2`, tenant, entity); got != "true" {
				t.Fatal("renamed restore trigger did not run")
			}
			// A transactional down/up round trip must restore names, not recreate identities.
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+q+`,public`); err != nil {
				t.Fatal(err)
			}
			for _, direction := range []string{"down", "up"} {
				body, err := fs.ReadFile(migrations.Postgres, "0039_entity_terminology."+direction+".sql")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, string(body)); err != nil {
					t.Fatalf("%s: %v", direction, err)
				}
			}
			var still string
			if err := tx.QueryRow(ctx, `SELECT id FROM content_entities WHERE tenant_id=$1 AND id=$2`, tenant, entity).Scan(&still); err != nil || still != entity {
				t.Fatalf("rollback/reapply replaced entity: %s %v", still, err)
			}
		})
	}
}
