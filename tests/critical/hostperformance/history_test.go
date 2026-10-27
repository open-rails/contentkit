//go:build hostperformance

package hostperformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Only the last day derives from real receipt reduction; earlier days contain
// explicitly synthetic prepared scalars, not a year of raw execution.
func (f *fixture) seedHistory() {
	if f.days <= 1 {
		return
	}
	t, ctx := f.t, f.t.Context()
	f.ch.phase.Store("historical_scalar_fixture")
	start := time.Now()
	from := f.day.AddDate(0, 0, 1-f.days)
	for off := 0; off < f.days-1; off += 7 {
		n := min(7, f.days-1-off)
		must(t, f.ch.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.popularity_daily (tenant,content_kind,domain,language,day,content_id,readers,eligible,version) SELECT tenant,content_kind,domain,language,addDays(toDate('%s'),toInt32(number)+%d),content_id,least(eligible,toUInt64(intDiv(readers*(80+(cityHash64(content_id)+intDiv(number+%d,7))%%41),100))),eligible,toUInt64(1) FROM (SELECT * FROM %s.popularity_daily FINAL WHERE day=toDate('%s')) AS base CROSS JOIN numbers(%d) AS dates`, f.db, from.Format(time.DateOnly), off, off, f.db, f.day.Format(time.DateOnly), n)))
	}
	for _, scope := range f.model.Kinds[f.kind].Popularity[0].ActivityScopes() {
		must(t, f.ch.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.popularity_daily_prepared (tenant,content_kind,domain,language,day,computed_at) SELECT ?,?,?,?,addDays(toDate(?),toInt32(number)),now64(6) FROM numbers(%d)`, f.db, f.days-1), f.tenant, f.kind, scope.Domain, scope.Language, from.Format(time.DateOnly)))
		must(t, f.ch.Exec(ctx, "INSERT INTO "+f.db+".measured_population_coverage (tenant,content_kind,domain,language,started_at) VALUES(?,?,?,?,?)", f.tenant, f.kind, scope.Domain, scope.Language, from))
	}
	_, e := f.pool.Exec(ctx, "UPDATE "+f.schema+".content_items SET created_at=$2,published_at=$2,first_published_at=$2 WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	_, e = f.pool.Exec(ctx, "UPDATE "+f.schema+".content_versions SET created_at=$2,published_at=$2 WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	_, e = f.pool.Exec(ctx, "UPDATE "+f.schema+".content_facets SET live_from=$2,published_at=$2 WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	_, e = f.pool.Exec(ctx, "UPDATE "+f.schema+".content_publication_requests SET scheduled_at=$2,completed_at=$2 WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	_, e = f.pool.Exec(ctx, "UPDATE "+f.schema+".content_access_history SET occurred_at=$2,snapshot=jsonb_set(snapshot,'{live_from}',to_jsonb($2::timestamptz)) WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	_, e = f.pool.Exec(ctx, "UPDATE "+f.schema+".content_access_history_start SET started_at=$2 WHERE tenant_id=$1", f.tenant, from)
	must(t, e)
	rows, e := f.ch.Query(ctx, "SELECT count(),uniqExact(content_id),uniqExact(tuple(domain,language)),min(day),max(day),sum(readers) FROM "+f.db+".popularity_daily FINAL")
	must(t, e)
	if !rows.Next() {
		t.Fatal("missing history rows")
	}
	var n, items, scopes, total uint64
	var first, last time.Time
	must(t, rows.Scan(&n, &items, &scopes, &first, &last, &total))
	rows.Close()
	expected := uint64(f.items * 2 * len(f.model.ContentFilters.Domains) * f.days)
	if n != expected || items != uint64(f.items) || !first.Equal(from) || !last.Equal(f.day) {
		t.Fatalf("historical count/cutoff mismatch rows%d/%d items%d bounds%s..%s", n, expected, items, first, last)
	}
	f.phases = append(f.phases, map[string]any{"phase": "historical_scalar_fixture", "seconds": time.Since(start).Seconds(), "canonical_rows": n, "works": items, "scopes": scopes, "from": from, "last_day": last, "sum_daily_readers_across_overlapping_scopes": total})
	save(t, f.out+"/phases.json", f.phases)
}
func (f *fixture) duringRefresh() {
	cases := f.httpCases()
	for _, c := range []int{1, 4, 8} {
		f.overlapLoad(fmt.Sprintf("during_refresh_%d", c), c, cases)
	}
}

func (f *fixture) overlapLoad(label string, concurrency int, cases []httpCase) {
	done, running := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); f.refreshObserved(label, running) }()
	select {
	case <-running:
	case <-done:
		f.t.Fatal("refresh ended before observed running state")
	}
	f.httpLoad(label, concurrency, cases)
	<-done
	var load struct {
		Samples []httpSample `json:"samples"`
	}
	b, e := os.ReadFile(filepath.Join(f.out, fmt.Sprintf("%s-load-%d.json", label, concurrency)))
	must(f.t, e)
	must(f.t, json.Unmarshal(b, &load))
	phase := f.phases[len(f.phases)-1]
	started := phase["attempted_at"].(time.Time)
	ended := phase["completed_at"].(time.Time)
	overlap := 0
	for _, s := range load.Samples {
		if s.StartedAt.Before(ended) && s.EndedAt.After(started) {
			overlap++
		}
	}
	save(f.t, filepath.Join(f.out, label+"-overlap.json"), map[string]any{"job": phase["job"], "job_started_at": started, "job_completed_at": ended, "requests": len(load.Samples), "overlapping_requests": overlap, "label": "mixed overlap when HTTP load outlasts refresh"})
	if overlap == 0 {
		f.t.Fatal("no measured requests overlapped refresh")
	}
}

// Aging this owned checkpoint invokes the ordinary worker's incomplete-final-
// build path. It is raw-day recomputation plus yearly ranking, not a claim that
// the fixture's closed calendar date is naturally provisional at wall time.
func (f *fixture) ageLastCheckpoint() {
	f.ch.phase.Store("age_owned_checkpoint")
	must(f.t, f.ch.Exec(f.t.Context(), "ALTER TABLE "+f.db+".popularity_daily_prepared DELETE WHERE tenant=? AND content_kind=? AND day=? SETTINGS mutations_sync=2", f.tenant, f.kind, f.day))
	// Replicated tables silently drop a block identical to a recent insert, so a
	// second aging would otherwise leave the day without any checkpoint.
	for _, scope := range f.model.Kinds[f.kind].Popularity[0].ActivityScopes() {
		must(f.t, f.ch.Exec(f.t.Context(), "INSERT INTO "+f.db+".popularity_daily_prepared (tenant,content_kind,domain,language,day,computed_at) SETTINGS insert_deduplicate=0 VALUES(?,?,?,?,?,?)", f.tenant, f.kind, scope.Domain, scope.Language, f.day, f.day.Add(23*time.Hour)))
	}
}
func (f *fixture) combinedRecompute() {
	f.ageLastCheckpoint()
	f.refresh("combined_recompute")
	f.verifyDaily()
	cases := f.httpCases()
	f.ageLastCheckpoint()
	f.overlapLoad("combined_recompute_load8", 8, cases)
	f.verifyDaily()
}
