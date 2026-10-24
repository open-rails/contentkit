package contentkit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
)

func offerRetirementFixture(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}

func insertOldOfferJob(t *testing.T, pool *pgxpool.Pool, schema, kind, state, args string) int64 {
	t.Helper()
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	stateType := pgx.Identifier{schema, "river_job_state"}.Sanitize()
	var id int64
	err := pool.QueryRow(t.Context(), `INSERT INTO `+table+` (kind,args,state,queue,max_attempts,finalized_at,metadata)
 VALUES ($1,$2::jsonb,$3::text::`+stateType+`,'old-offers',3,
 CASE WHEN $3 IN ('completed','cancelled','discarded') THEN '2026-01-01T00:00:00Z'::timestamptz ELSE NULL END,
 '{"preserve":"audit evidence"}') RETURNING id`, kind, args, state).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func oldOfferRows(t *testing.T, pool *pgxpool.Pool, schema string) map[int64]map[string]any {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT id,to_jsonb(j) FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()+` j ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]map[string]any{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		out[id] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOfferJobRetirementPreservesHistoryAndScopes(t *testing.T) {
	pool, schema := offerRetirementFixture(t)
	targets := map[int64]bool{}
	states := []string{"available", "pending", "retryable", "scheduled", "completed", "cancelled", "discarded"}
	for _, kind := range []string{"contentkit_offer_sync", "contentkit_offer_reconcile"} {
		for _, state := range states {
			for _, tenant := range []string{"current", "legacy", "other"} {
				id := insertOldOfferJob(t, pool, schema, kind, state, `{"tenant":"`+tenant+`"}`)
				targets[id] = tenant != "other" && (state == "available" || state == "pending" || state == "retryable" || state == "scheduled")
			}
		}
	}
	// Neither a different tenant/kind nor a missing or malformed tenant can be
	// mistaken for one of this application's retired jobs.
	insertOldOfferJob(t, pool, schema, "contentkit_offer_sync", "running", `{"tenant":"other"}`)
	insertOldOfferJob(t, pool, schema, "contentkit_content_publish_due", "running", `{"tenant":"current"}`)
	for _, args := range []string{`{}`, `{"tenant":null}`, `{"tenant":["current"]}`, `{"tenant":123}`} {
		insertOldOfferJob(t, pool, schema, "contentkit_offer_sync", "available", args)
	}
	before := oldOfferRows(t, pool, schema)
	if err := preflightOfferJobs(t.Context(), pool, schema, []string{"current", "legacy", "123"}); err != nil {
		t.Fatal(err)
	}
	if got := oldOfferRows(t, pool, schema); !reflect.DeepEqual(before, got) {
		t.Fatal("preflight changed job records")
	}
	if err := retireOfferJobs(t.Context(), pool, schema, []string{"current", "legacy", "123"}); err != nil {
		t.Fatal(err)
	}
	after := oldOfferRows(t, pool, schema)
	for id, row := range before {
		if targets[id] {
			if after[id]["state"] != "cancelled" || after[id]["finalized_at"] == nil {
				t.Fatalf("pending job %d was not finalized as cancelled: %+v", id, after[id])
			}
			row["state"], row["finalized_at"] = after[id]["state"], after[id]["finalized_at"]
		}
		if !reflect.DeepEqual(row, after[id]) {
			t.Fatalf("job %d lost history or unrelated state: before=%+v after=%+v", id, row, after[id])
		}
	}
	if err := retireOfferJobs(t.Context(), pool, schema, []string{"current", "legacy"}); err != nil {
		t.Fatal(err)
	}
	if got := oldOfferRows(t, pool, schema); !reflect.DeepEqual(after, got) {
		t.Fatal("repeated retirement changed retained records")
	}
}

func TestOfferJobRetirementRefusesRunningWriterAtomically(t *testing.T) {
	pool, schema := offerRetirementFixture(t)
	insertOldOfferJob(t, pool, schema, "contentkit_offer_sync", "available", `{"tenant":"current"}`)
	running := insertOldOfferJob(t, pool, schema, "contentkit_offer_reconcile", "scheduled", `{"tenant":"legacy"}`)
	if err := preflightOfferJobs(t.Context(), pool, schema, []string{"current", "legacy"}); err != nil {
		t.Fatal(err)
	}
	// A worker can start after the read-only preflight. The post-migration
	// retirement must recheck under row locks and refuse without partial work.
	if _, err := pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{schema, "river_job"}.Sanitize()+` SET state='running' WHERE id=$1`, running); err != nil {
		t.Fatal(err)
	}
	before := oldOfferRows(t, pool, schema)
	for _, check := range []func() error{
		func() error { return preflightOfferJobs(t.Context(), pool, schema, []string{"current", "legacy"}) },
		func() error { return retireOfferJobs(t.Context(), pool, schema, []string{"current", "legacy"}) },
	} {
		if err := check(); err == nil || !strings.Contains(err.Error(), "stop old ContentKit workers") {
			t.Fatalf("expected actionable running-writer refusal, got %v", err)
		}
		if got := oldOfferRows(t, pool, schema); !reflect.DeepEqual(before, got) {
			t.Fatal("refusal partially cancelled pending work")
		}
	}
}

func TestOfferJobRetirementWithoutExistingRiver(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	for _, check := range []func() error{
		func() error { return preflightOfferJobs(t.Context(), pool, schema, []string{"current"}) },
		func() error { return retireOfferJobs(t.Context(), pool, schema, []string{"current"}) },
	} {
		if err := check(); err != nil {
			t.Fatalf("fresh installation without River jobs: %v", err)
		}
	}
}
