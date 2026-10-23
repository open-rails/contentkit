package entities

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// An opt-in receipt against the production directory query: ordinary taxonomy
// and a small timed rail population, with no workers or counter projections.
func TestRuntimeEntityQueryPlans(t *testing.T) {
	if os.Getenv("CONTENTKIT_QUERY_PLANS") == "" {
		t.Skip("set CONTENTKIT_QUERY_PLANS to measure query plans")
	}
	f := newFixture(t)
	var inputs []EntityInput
	for i := 0; i < 100; i++ {
		inputs = append(inputs, EntityInput{Kind: "tag", Slug: fmt.Sprintf("tag-%d", i)})
	}
	entities := f.create(inputs...)
	f.exec(`INSERT INTO {s}.content_items(tenant_id,id,kind,state)
 SELECT $1,md5(i::text)::uuid::text,'gallery','published' FROM generate_series(1,5000) i`, tenant)
	f.exec(`INSERT INTO {s}.content_versions(tenant_id,id,content_id,languages)
 SELECT tenant_id,md5(id)::uuid::text,id,ARRAY['en'] FROM {s}.content_items WHERE tenant_id=$1`, tenant)
	f.exec(`INSERT INTO {s}.content_memberships(tenant_id,entity_id,content_id)
 SELECT c.tenant_id,g.id,c.id FROM {s}.content_items c JOIN {s}.content_entities g
 ON g.tenant_id=c.tenant_id AND g.slug='tag-'||(abs(hashtext(c.id)::bigint)%100)::text WHERE c.tenant_id=$1`, tenant)
	f.exec(`INSERT INTO {s}.content_facets(tenant_id,content_id,version_id,kind,live_from,languages,access_level,entity_ids,published_at)
 SELECT v.tenant_id,v.content_id,v.id,'gallery',now()-interval '1 day',v.languages,'public',ARRAY[m.entity_id],now()-interval '1 day'
 FROM {s}.content_versions v JOIN {s}.content_memberships m ON m.tenant_id=v.tenant_id AND m.content_id=v.content_id WHERE v.tenant_id=$1`, tenant)
	measure := func(label string) {
		t.Helper()
		f.exec(`ANALYZE {s}.content_facets; ANALYZE {s}.content_memberships; ANALYZE {s}.content_entities`)
		for _, sort := range []Sort{SortName, SortCount} {
			query, err := f.s.compile(ListOptions{Kind: "tag", Sort: sort, Limit: 20}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := f.pool.QueryRow(f.ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query.sql, query.args).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct {
				Execution float64         `json:"Execution Time"`
				Planning  float64         `json:"Planning Time"`
				Plan      json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s sort=%s entities=100 versions=5000 execution_ms=%.3f planning_ms=%.3f plan=%s", label, sort, plan[0].Execution, plan[0].Planning, plan[0].Plan)
		}
		page, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Sort: SortCount, Limit: 20})
		if err != nil || len(page.Entities) != 20 || page.Entities[0].Count == 0 {
			t.Fatalf("directory result: %+v %v", page, err)
		}
	}
	measure("timeless")
	f.exec(`UPDATE {s}.content_memberships SET active_from=now()-interval '3 days' WHERE tenant_id=$1 AND entity_id=ANY($2::text[])`, tenant, []string{string(entities[0].ID), string(entities[1].ID)})
	measure("timed")
}
