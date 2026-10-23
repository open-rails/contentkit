package discovery

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/access"
)

type listingPlanTrace struct {
	mu    sync.Mutex
	query pgx.TraceQueryStartData
}

func (t *listingPlanTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(q.SQL, "SELECT f.content_id,") && strings.Contains(q.SQL, "GROUP BY") {
		t.mu.Lock()
		t.query = q
		t.mu.Unlock()
	}
	return ctx
}
func (*listingPlanTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRuntimeDiscoveryQueryPlans(t *testing.T) {
	if os.Getenv("CONTENTKIT_QUERY_PLANS") == "" {
		t.Skip("set CONTENTKIT_QUERY_PLANS to measure query plans")
	}
	f := newFixture(t, nil, "")
	f.entity("rail", "featured")
	entity := f.ids["featured"]
	f.exec(`INSERT INTO {s}.content_items(tenant_id,id,kind,state)
 SELECT $1,md5(i::text)::uuid::text,'gallery','published' FROM generate_series(1,5000) i`, tenant)
	f.exec(`INSERT INTO {s}.content_versions(tenant_id,id,content_id,languages,state)
 SELECT tenant_id,md5(id)::uuid::text,id,ARRAY['en'],'published' FROM {s}.content_items WHERE tenant_id=$1`, tenant)
	f.exec(`INSERT INTO {s}.content_memberships(tenant_id,entity_id,content_id)
 SELECT tenant_id,$2,id FROM {s}.content_items WHERE tenant_id=$1 ORDER BY id LIMIT 100`, tenant, entity)
	f.exec(`INSERT INTO {s}.content_facets(tenant_id,content_id,version_id,kind,live_from,languages,access_level,entity_ids,published_at)
 SELECT v.tenant_id,v.content_id,v.id,'gallery',now()-interval '1 day',v.languages,'public',
 coalesce(ARRAY(SELECT m.entity_id FROM {s}.content_memberships m WHERE m.tenant_id=v.tenant_id AND m.content_id=v.content_id),'{}'),now()-interval '1 day'
 FROM {s}.content_versions v WHERE v.tenant_id=$1`, tenant)
	trace := &listingPlanTrace{}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f.svc.o.Pool = pool
	for _, label := range []string{"timeless", "timed"} {
		if label == "timed" {
			f.exec(`UPDATE {s}.content_memberships SET active_from=now()-interval '3 days' WHERE tenant_id=$1 AND entity_id=$2`, tenant, entity)
		}
		f.exec(`ANALYZE {s}.content_facets; ANALYZE {s}.content_memberships`)
		_, err := f.svc.List(f.ctx, ListOptions{Reader: Reader{Actor: access.Actor{Anonymous: true}, Host: "main.test"}, Filter: Filter{Kind: "gallery", Entities: []string{entity}}, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		trace.mu.Lock()
		query := trace.query
		trace.mu.Unlock()
		if query.SQL == "" {
			t.Fatal("production listing query was not captured")
		}
		var raw []byte
		if err := pool.QueryRow(f.ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query.SQL, query.Args...).Scan(&raw); err != nil {
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
		t.Logf("%s filtered production listing versions=5000 members=100 execution_ms=%.3f planning_ms=%.3f plan=%s", label, plan[0].Execution, plan[0].Planning, plan[0].Plan)
	}
}
