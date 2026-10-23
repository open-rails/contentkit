package catalog

import (
	"testing"
)

func TestRetireOpenCountersDropsStaleFacetPopularity(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Old cached ranks", "en"))
	f.publish("staff", id, nil)
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_facets")+` SET popularity='{"score":{"7d":99},"rating":{"30d":0.9}}', view_baseline=500 WHERE content_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	columns := func() map[string]bool {
		t.Helper()
		rows, err := f.pool.Query(f.ctx, `SELECT table_name||'.'||column_name FROM information_schema.columns
 WHERE table_schema=$1 AND table_name IN ('content_items','content_facets') AND column_name IN ('view_baseline','popularity')`, f.schema)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out[c] = true
		}
		return out
	}
	if err := f.s.RetireOpenCounters(f.ctx, false); err == nil {
		t.Fatal("retirement must require stopped legacy writers")
	}
	if got := columns(); len(got) != 3 {
		t.Fatalf("refused retirement changed the schema: %v", got)
	}
	if err := RetireOpenCounters(f.ctx, f.pool, f.schema, true); err != nil {
		t.Fatal(err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("retired columns remain: %v", got)
	}
	if err := f.s.RetireOpenCounters(f.ctx, true); err != nil {
		t.Fatalf("retirement must be idempotent: %v", err)
	}
	// Retirement must retain the runtime read view, not drop dependencies.
	var current int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_current_facets")+` WHERE tenant_id=$1 AND content_id=$2`, tenant, id).Scan(&current); err != nil || current != 1 {
		t.Fatalf("runtime view after retirement: %d %v", current, err)
	}
	// Current writers never needed the retired columns.
	next := f.create("staff", gallery("Published after retirement", "en"))
	f.publish("staff", next, nil)
	var facets int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_facets")+` WHERE content_id=ANY($1)`, []string{id, next}).Scan(&facets); err != nil || facets != 2 {
		t.Fatalf("facets after retirement: %d %v", facets, err)
	}
}
