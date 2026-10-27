//go:build hostperformance

package hostperformance

import (
	"fmt"
	"strings"
	"time"
)

// Fixture generation is excluded from worker timings. One normally published
// edition supplies the current schema shape; synthetic identities are explicit.
func (f *fixture) cloneCatalog() {
	t, ctx, s := f.t, f.t.Context(), f.schema
	i, v := f.template.ID, f.template.Versions[0].ID
	var r string
	must(t, f.pool.QueryRow(ctx, "SELECT release_id::text FROM "+s+".content_versions WHERE id=$1 AND tenant_id=$2", v, f.tenant).Scan(&r))
	langs := []string{}
	for _, l := range f.model.Languages {
		langs = append(langs, "'"+l+"'")
	}
	l, d := len(langs), len(f.model.ContentFilters.Domains)
	_, e := f.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.benchmark_items AS SELECT n::int item,
 '00000001-0000-7000-8000-'||lpad(to_hex(n),12,'0') id,
 '00000002-0000-7000-8000-'||lpad(to_hex(n),12,'0') version,
 '00000003-0000-7000-8000-'||lpad(to_hex(n),12,'0') request,
 'A'||lpad(upper(to_hex(n)),8,'0') code,
 (ARRAY[%s])[(n%%%d)+1] language,(n/%d)%%%d domain_number,
 CASE WHEN n%%5=0 THEN 'tier' ELSE 'public' END level FROM generate_series(0,%d)n`, s, strings.Join(langs, ","), l, l, d, f.items-1))
	must(t, e)
	ent := fmt.Sprintf("CASE b.domain_number WHEN 1 THEN ARRAY['%s','%s'] WHEN 2 THEN ARRAY['%s','%s'] ELSE ARRAY['%s'] END", f.tagIDs["bench"], f.tagIDs["yaoi"], f.tagIDs["bench"], f.tagIDs["furry"], f.tagIDs["bench"])
	if f.app == "hentai0" {
		ent = fmt.Sprintf("CASE WHEN b.item%%10=0 THEN ARRAY['%s','%s'] ELSE ARRAY['%s'] END", f.tagIDs["bench"], f.tagIDs["restricted"], f.tagIDs["bench"])
	}
	clone := func(table, where, override string) {
		rows, e := f.pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND is_generated='NEVER' AND is_identity='NO' ORDER BY ordinal_position`, s, table)
		must(t, e)
		var cols []string
		for rows.Next() {
			var col string
			must(t, rows.Scan(&col))
			cols = append(cols, `"`+col+`"`)
		}
		rows.Close()
		must(t, rows.Err())
		replaced := fmt.Sprintf("replace(replace(replace(to_jsonb(t)::text,'%s',b.id),'%s',b.version),'%s',b.request)::jsonb", i, v, r)
		q := fmt.Sprintf("INSERT INTO %s.%s (%s) SELECT x.%s FROM %s.%s t CROSS JOIN %s.benchmark_items b CROSS JOIN LATERAL jsonb_populate_record(NULL::%s.%s,%s || jsonb_build_object(%s)) x WHERE %s", s, table, strings.Join(cols, ","), strings.Join(cols, ",x."), s, table, s, s, table, replaced, override, where)
		_, e = f.pool.Exec(ctx, q)
		must(t, e)
	}
	day := "'" + f.day.Format(time.RFC3339) + "'::timestamptz"
	rule := "jsonb_build_object('level',b.level,'tiers',CASE WHEN b.level='tier' THEN jsonb_build_array('premium') ELSE '[]'::jsonb END)"
	clone("content_items", "t.id='"+i+"'", "'access',"+rule+",'created_at',"+day+",'published_at',"+day+",'first_published_at',"+day)
	clone("content_versions", "t.id='"+v+"'", "'release_id',NULL,'languages',jsonb_build_array(b.language),'access',"+rule+",'released_access',"+rule+",'published_at',"+day+",'created_at',"+day)
	clone("content_publication_requests", "t.id='"+r+"'", "'completed_at',"+day+",'scheduled_at',"+day)
	_, e = f.pool.Exec(ctx, "UPDATE "+s+".content_versions v SET release_id=b.request::uuid FROM "+s+".benchmark_items b WHERE v.id=b.version AND v.tenant_id=$1", f.tenant)
	must(t, e)
	clone("content_version_text", "t.version_id='"+v+"'", "'language',b.language,'title','Benchmark work '||b.item,'slug','benchmark-'||b.item,'body','Synthetic benchmark body '||b.item")
	clone("content_codes", "t.content_id='"+i+"'", "'code',b.code,'slug','benchmark-'||b.item,'slugs',jsonb_build_object(b.language,'benchmark-'||b.item)")
	clone("content_facets", "t.version_id='"+v+"'", "'languages',jsonb_build_array(b.language),'access_level',b.level,'access_tiers',CASE WHEN b.level='tier' THEN jsonb_build_array('premium') ELSE '[]'::jsonb END,'entity_ids',to_jsonb("+ent+"),'live_from',"+day+",'published_at',"+day)
	_, e = f.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_memberships (tenant_id,content_id,entity_id) SELECT $1,b.id,unnest(%s) FROM %s.benchmark_items b`, s, ent, s), f.tenant)
	must(t, e)
	_, e = f.pool.Exec(ctx, "DELETE FROM "+s+".content_items WHERE tenant_id=$1 AND id=$2", f.tenant, i)
	must(t, e)
	_, e = f.pool.Exec(ctx, "DELETE FROM "+s+".content_access_history WHERE tenant_id=$1", f.tenant)
	must(t, e)
	_, e = f.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_access_history (tenant_id,content_id,version_id,occurred_at,snapshot) SELECT $1,b.id,b.version,$2,jsonb_build_object('kind',$3::text,'live_from',$2::timestamptz,'deleted',false,'languages',jsonb_build_array(b.language),'level',b.level,'tiers',CASE WHEN b.level='tier' THEN jsonb_build_array('premium') ELSE '[]'::jsonb END,'releases','[]'::jsonb) FROM %s.benchmark_items b`, s, s), f.tenant, f.day, f.kind)
	must(t, e)
	_, e = f.pool.Exec(ctx, "INSERT INTO "+s+".content_access_history_start(tenant_id,started_at) VALUES($1,$2) ON CONFLICT(tenant_id) DO UPDATE SET started_at=excluded.started_at", f.tenant, f.day)
	must(t, e)
	_, e = f.pool.Exec(ctx, "ANALYZE "+s+".content_facets; ANALYZE "+s+".content_versions; ANALYZE "+s+".content_version_text; ANALYZE "+s+".content_access_history")
	must(t, e)
}
func (f *fixture) seedRawDay() {
	t, ctx := f.t, f.t.Context()
	f.ch.phase.Store("raw_fixture")
	start := time.Now()
	l, d := len(f.model.Languages), len(f.model.ContentFilters.Domains)
	cells := l * d
	perCell := f.items / cells
	hot := max(10, perCell/100)
	array := func(ss []string) string {
		out := []string{}
		for _, s := range ss {
			out = append(out, "'"+s+"'")
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	domains := []string{}
	for _, v := range f.model.ContentFilters.Domains {
		domains = append(domains, v.Name)
	}
	// 10 distinct works per user: five hot and five cold. Scope overlap never
	// creates extra raw receipts; empty-language scopes read the same input.
	itemExpr := fmt.Sprintf("if(slot<5,(person*5+slot)%%%d,%d+(person*5+slot-5)%%%d)*%d+(intDiv(person,%d)%%%d)*%d+person%%%d", hot, hot, perCell-hot, cells, l, d, l, l)
	idSQL := func(prefix int, expr string) string {
		return fmt.Sprintf("concat('%08x-0000-7000-8000-',leftPad(lower(hex(%s)),12,'0'))", prefix, expr)
	}
	total := f.users * 10 * 6
	for off := 0; off < total; off += 300000 {
		n := min(300000, total-off)
		q := fmt.Sprintf(`INSERT INTO %s.engagement_segments (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement)
 WITH intDiv(number,6) pair,intDiv(pair,10) person,pair%%10 slot,intDiv(number%%6,2) receipt,2-number%%2 rev,%s item,
 %s[intDiv(person,%d)%%%d+1] domain,%s[person%%%d+1] language
 SELECT '%s','%s',%s,%s,'user',%s,%s,rev,%s,%s,toDateTime64('%s',6,'UTC'),toUInt32(if(person%%10=0,100,if(rev=2,400,200))),[toUInt32(0)],map(),100,100,'',concat('{"viewer_access":"full","eligibility_known":true,"domain":"',domain,'","language":"',language,'"}'),'{}' FROM numbers(%d,%d)`, f.db, itemExpr, array(domains), l, d, array(f.model.Languages), l, f.tenant, f.kind, idSQL(1, "item"), idSQL(2, "item"), idSQL(4, "person"), idSQL(5, "pair*3+receipt"), idSQL(6, "pair"), idSQL(7, "pair*3+receipt"), f.day.Add(12*time.Hour).Format("2006-01-02 15:04:05"), off, n)
		must(t, f.ch.Exec(ctx, q))
	}
	q := fmt.Sprintf(`INSERT INTO %s.measured_population_observations (tenant,subject_kind,subject,content_kind,observation_id,observed_at,domain,language,eligible_tiers,eligibility_known) WITH intDiv(number,4) person,intDiv(number%%4,2) observation SELECT '%s','user',%s,'%s',%s,toDateTime64('%s',6,'UTC')+toIntervalSecond(observation),%s[intDiv(person,%d)%%%d+1],%s[person%%%d+1],if(person%%2=0 AND observation=1,['premium'],CAST([],'Array(String)')),true FROM numbers(%d)`, f.db, f.tenant, idSQL(4, "person"), f.kind, idSQL(8, "person*2+observation"), f.day.Add(11*time.Hour).Format("2006-01-02 15:04:05"), array(domains), l, d, array(f.model.Languages), l, f.users*4)
	must(t, f.ch.Exec(ctx, q))
	for _, scope := range f.model.Kinds[f.kind].Popularity[0].ActivityScopes() {
		must(t, f.ch.Exec(ctx, "INSERT INTO "+f.db+".measured_population_coverage (tenant,content_kind,domain,language,started_at) VALUES(?,?,?,?,?)", f.tenant, f.kind, scope.Domain, scope.Language, f.day))
	}
	f.phases = append(f.phases, map[string]any{"phase": "raw_fixture", "seconds": time.Since(start).Seconds(), "delivered_receipts": total, "population_observations": f.users * 4})
	save(t, f.out+"/phases.json", f.phases)
}
func (f *fixture) verifyDaily() {
	rows, e := f.ch.Query(f.t.Context(), "SELECT domain,language,count(),sum(readers),max(eligible) FROM "+f.db+".popularity_daily FINAL GROUP BY domain,language ORDER BY domain,language")
	must(f.t, e)
	var summary []map[string]any
	for rows.Next() {
		var d, l string
		var n, u, p uint64
		must(f.t, rows.Scan(&d, &l, &n, &u, &p))
		summary = append(summary, map[string]any{"domain": d, "language": l, "items": n, "qualified": u, "max_eligible": p})
	}
	must(f.t, rows.Err())
	rows.Close()
	save(f.t, f.out+"/daily-counts.json", summary)
	want := len(f.model.Kinds[f.kind].Popularity[0].ActivityScopes())
	if len(summary) != want {
		f.t.Fatalf("daily scope count%d want%d", len(summary), want)
	}
}
