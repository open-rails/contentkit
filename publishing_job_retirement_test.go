package contentkit

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPublishingJobsRetireWithoutCancellingMediaImports(t *testing.T) {
	pool, schema := offerRetirementFixture(t)
	pending := insertOldOfferJob(t, pool, schema, "contentkit_publishing_operation", "available", `{"tenant":"current"}`)
	insertOldOfferJob(t, pool, schema, "contentkit_publishing_reconcile", "completed", `{"tenant":"current"}`)
	insertOldOfferJob(t, pool, schema, "contentkit_publishing_operation", "available", `{"tenant":"other"}`)
	insertOldOfferJob(t, pool, schema, "contentkit_version_import", "available", `{"tenant":"current"}`)
	before := oldOfferRows(t, pool, schema)
	if err := preflightPublishingJobs(t.Context(), pool, schema, []string{"current"}); err != nil {
		t.Fatal(err)
	}
	if err := retirePublishingJobs(t.Context(), pool, schema, []string{"current"}); err != nil {
		t.Fatal(err)
	}
	after := oldOfferRows(t, pool, schema)
	for id, row := range before {
		if id == pending {
			if after[id]["state"] != "cancelled" || after[id]["finalized_at"] == nil {
				t.Fatal("coordinator not retired")
			}
			row["state"], row["finalized_at"] = after[id]["state"], after[id]["finalized_at"]
		}
		if !reflect.DeepEqual(row, after[id]) {
			t.Fatalf("unrelated media job or history changed: %d", id)
		}
	}
	// Recheck under lock: a late-starting old coordinator is not discarded.
	late := insertOldOfferJob(t, pool, schema, "contentkit_publishing_operation", "available", `{"tenant":"current"}`)
	if _, err := pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{schema, "river_job"}.Sanitize()+` SET state='running' WHERE id=$1`, late); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{
		func() error { return preflightPublishingJobs(t.Context(), pool, schema, []string{"current"}) },
		func() error { return retirePublishingJobs(t.Context(), pool, schema, []string{"current"}) },
	} {
		if err := check(); err == nil || !strings.Contains(err.Error(), "stop old ContentKit workers") {
			t.Fatalf("running coordinator=%v", err)
		}
	}
}
