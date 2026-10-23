package offers

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	riverhelpers "github.com/open-rails/helpers/river"
)

// A running sync has already read its revision. An edit can arrive before the
// job is marked completed, so it must retain one waiting follow-up rather than
// depend on the next periodic reconcile to discover the lost revision.
func TestReconcileQueuesFollowupWhileTargetSyncRuns(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	riverSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	cfg := kinds.Config{Languages: []string{"en"},
		EntityKinds: map[string]kinds.EntityKind{"channel": {Route: "c", Sellable: true}}}
	billing := &heldForPolicyTest{}
	gate, err := access.NewGate(access.GateConfig{Tenant: "policy", Kinds: cfg, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: "policy", Kinds: cfg, Billing: billing, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if s.client, err = river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema}); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	entities := pgx.Identifier{schema, "content_entities"}.Sanitize()
	offers := pgx.Identifier{schema, "content_offers"}.Sanitize()
	jobs := pgx.Identifier{riverSchema, "river_job"}.Sanitize()
	target := Target{Kind: EntityTarget, ID: contentref.NewID()}
	exec(`INSERT INTO `+entities+` (tenant_id,id,kind,slug) VALUES ('policy',$1,'channel','studio')`, target.ID)
	// A previous offer may have a larger revision than the new active offer.
	// Taking only max(revision) would suppress the active offer's early edits.
	exec(`INSERT INTO `+offers+` (tenant_id,id,target_kind,target_id,terms,state,revision,synced_revision)
		VALUES ('policy',$1,'group',$2,'{"amount":0,"currency":"USD"}','withdrawn',10,10)`, contentref.NewID(), target.ID)
	if _, err := s.Put(ctx, []Put{{Target: target, Terms: Terms{Currency: "USD"}}}); err != nil {
		t.Fatal(err)
	}
	// Represent the existing worker after it took the original revision. No
	// sleeps or live worker timing are needed to exercise the real dedup path.
	exec(`UPDATE ` + jobs + ` SET state='running', attempt=1, attempted_at=now()`)
	changed, err := s.Put(ctx, []Put{{Target: target, Terms: Terms{Amount: 4_990_000, Currency: "USD"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Revision != 2 {
		t.Fatalf("offer after edit: %+v, want revision 2", changed)
	}
	for range 3 {
		if err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var running, waiting int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='running'), count(*) FILTER (WHERE state='available') FROM `+jobs+` WHERE kind='contentkit_offer_sync'`).Scan(&running, &waiting); err != nil {
		t.Fatal(err)
	}
	if running != 1 || waiting != 1 {
		t.Fatalf("syncs after revision changed: %d running, %d waiting; want 1 running and 1 waiting", running, waiting)
	}
	var revisions []int64
	if err := pool.QueryRow(ctx, `SELECT array_agg((args->>'revision')::bigint ORDER BY (args->>'revision')::bigint) FROM `+jobs+` WHERE kind='contentkit_offer_sync'`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || revisions[0] != 11 || revisions[1] != 12 {
		t.Fatalf("queued target revisions: %v, want [11 12]", revisions)
	}
	// Completion of the old revision must not add another waiting job.
	exec(`UPDATE ` + jobs + ` SET state='completed', finalized_at=now() WHERE state='running'`)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_offer_sync' AND state='available'`).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 {
		t.Fatalf("waiting follow-up syncs after old job completes: got %d, want 1", waiting)
	}
}
