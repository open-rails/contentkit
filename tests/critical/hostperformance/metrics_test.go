//go:build hostperformance

package hostperformance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type pgTiming struct {
	Phase, SQL          string
	Calls               int
	Seconds, MaxSeconds float64
}
type pgTrace struct {
	phase *atomic.Value
	mu    sync.Mutex
	stats map[string]*pgTiming
}
type traceStart struct {
	at         time.Time
	phase, sql string
}
type traceKey struct{}

func (p *pgTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceStart{time.Now(), fmt.Sprint(p.phase.Load()), data.SQL})
}
func (p *pgTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	s, ok := ctx.Value(traceKey{}).(traceStart)
	if !ok {
		return
	}
	elapsed := time.Since(s.at).Seconds()
	key := fmt.Sprintf("%s:%x", s.phase, sha256.Sum256([]byte(s.sql)))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stats == nil {
		p.stats = map[string]*pgTiming{}
	}
	r := p.stats[key]
	if r == nil {
		r = &pgTiming{Phase: s.phase, SQL: s.sql}
		p.stats[key] = r
	}
	r.Calls++
	r.Seconds += elapsed
	r.MaxSeconds = max(r.MaxSeconds, elapsed)
}
func (f *fixture) record() {
	t, ctx := f.t, context.Background()
	f.ch.mu.Lock()
	queries := append([]map[string]any{}, f.ch.queries...)
	f.ch.mu.Unlock()
	save(t, filepath.Join(f.out, "queries.json"), queries)
	rawByPhase := map[string]int{}
	for _, q := range queries {
		phase := fmt.Sprint(q["phase"])
		sql := fmt.Sprint(q["sql"])
		if strings.Contains(sql, ".engagement_segments") || strings.Contains(sql, ".measured_population_observations") {
			rawByPhase[phase]++
			if phase == "finalized_reuse" || phase == "historical_presets" || strings.HasPrefix(phase, "during_refresh_") || strings.Contains(phase, "http") {
				t.Errorf("unexpected raw-source query in%s", phase)
			}
		}
	}
	save(t, filepath.Join(f.out, "raw-query-boundary.json"), rawByPhase)

	f.pg.mu.Lock()
	save(t, filepath.Join(f.out, "postgres-query-times.json"), f.pg.stats)
	f.pg.mu.Unlock()
	if e := f.ch.Conn.Exec(ctx, "SYSTEM FLUSH LOGS"); e != nil {
		t.Log("query log flush:", e)
		return
	}
	rows, e := f.ch.Conn.Query(ctx, `SELECT query_id,query_duration_ms,read_rows,read_bytes,written_rows,memory_usage,ProfileEvents['UserTimeMicroseconds'],ProfileEvents['SystemTimeMicroseconds'],ProfileEvents['OSReadBytes'] FROM system.query_log WHERE query_id LIKE ? AND type='QueryFinish' ORDER BY event_time_microseconds`, f.schema+"_%")
	if e != nil {
		t.Log("query log read:", e)
		return
	}
	defer rows.Close()
	var metrics []map[string]any
	for rows.Next() {
		var id string
		var ms, n, b, w, m, u, s, io uint64
		must(t, rows.Scan(&id, &ms, &n, &b, &w, &m, &u, &s, &io))
		metrics = append(metrics, map[string]any{"id": id, "duration_ms": ms, "read_rows": n, "read_bytes": b, "written_rows": w, "peak_memory_bytes": m, "cpu_user_us": u, "cpu_system_us": s, "os_read_bytes": io})
	}
	must(t, rows.Err())
	save(t, filepath.Join(f.out, "clickhouse-metrics.json"), metrics)
}
