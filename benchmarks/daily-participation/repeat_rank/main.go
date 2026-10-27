package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/open-rails/contentkit/signal"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func save(p string, x any) {
	b, e := json.MarshalIndent(x, "", "  ")
	must(e)
	must(os.WriteFile(p, append(b, '\n'), 0600))
}
func main() {
	var db, out string
	var repeats int
	flag.StringVar(&db, "database", "", "owned ck261 database")
	flag.StringVar(&out, "output", "", "new output directory")
	flag.IntVar(&repeats, "repeats", 12, "repeats")
	flag.Parse()
	if !strings.HasPrefix(db, "ck261_actual_") || out == "" {
		panic("owned database and output required")
	}
	must(os.Mkdir(out, 0700))
	ctx := context.Background()
	c, e := clickhouse.Open(&clickhouse.Options{Addr: []string{os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR")}, Auth: clickhouse.Auth{Username: os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), Password: os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")}, Settings: clickhouse.Settings{"max_threads": 2, "max_memory_usage": 1073741824, "use_query_cache": 0, "log_queries": 1}, ReadTimeout: 180 * time.Second})
	must(e)
	defer c.Close()
	st, e := signal.NewStore(c, db)
	must(e)
	var baseline []signal.PopularHit
	var logs []map[string]any
	var ids []string
	for run := 0; run < repeats; run++ {
		qid := fmt.Sprintf("%s_actual_repeat_%d_%d", db, time.Now().Unix(), run)
		ids = append(ids, qid)
		start := time.Now()
		hits, e := st.DailyPopular(clickhouse.Context(ctx, clickhouse.WithQueryID(qid)), "bench261", "gallery", signal.AllTime(), signal.ActivityScope{Domain: "main", Language: "en"}, 100000)
		must(e)
		methodSeconds := time.Since(start).Seconds()
		if len(hits) != 100000 {
			panic(fmt.Sprintf("expected 100000 IDs, got %d", len(hits)))
		}
		orderedIDs := sha256.New()
		seen := make(map[string]bool, len(hits))
		for i, h := range hits {
			if seen[h.ContentID] {
				panic("duplicate ID")
			}
			seen[h.ContentID] = true
			fmt.Fprintln(orderedIDs, h.ContentID)
			if i > 0 && (hits[i-1].Score < h.Score || (hits[i-1].Score == h.Score && hits[i-1].ContentID > h.ContentID)) {
				panic("unsorted result")
			}
		}
		save(filepath.Join(out, fmt.Sprintf("run-%02d.json", run)), hits)
		changes := []map[string]any{}
		before := map[string]float64{}
		for _, h := range baseline {
			before[h.ContentID] = h.Score
		}
		for i, h := range hits {
			if baseline != nil && baseline[i].ContentID != h.ContentID {
				changes = append(changes, map[string]any{"position": i, "previous_id": baseline[i].ContentID, "current_id": h.ContentID, "previous_item_score_before": baseline[i].Score, "previous_item_score_now": float64(0), "current_item_score_before": before[h.ContentID], "current_item_score_now": h.Score})
			}
		}
		nowScores := map[string]float64{}
		for _, h := range hits {
			nowScores[h.ContentID] = h.Score
		}
		for _, r := range changes {
			r["previous_item_score_now"] = nowScores[r["previous_id"].(string)]
		}
		logs = append(logs, map[string]any{"run": run, "query_id": qid, "wall_seconds": time.Since(start).Seconds(), "method_wall_seconds": methodSeconds, "ordered_ids_sha256": fmt.Sprintf("%x", orderedIDs.Sum(nil)), "ids": len(hits), "order_changed_positions": len(changes)})
		if len(changes) > 0 {
			save(filepath.Join(out, fmt.Sprintf("changes-%02d.json", run)), changes)
		}
		if baseline == nil {
			baseline = hits
		}
		save(filepath.Join(out, "summary.json"), logs)
	}
	must(c.Exec(ctx, "SYSTEM FLUSH LOGS"))
	rows, e := c.Query(ctx, `SELECT query_id,query_duration_ms,read_rows,read_bytes,memory_usage,ProfileEvents['UserTimeMicroseconds'],ProfileEvents['SystemTimeMicroseconds'],ProfileEvents['OSReadBytes'] FROM system.query_log WHERE query_id IN ? AND type='QueryFinish'`, ids)
	must(e)
	var metrics []map[string]any
	for rows.Next() {
		var id string
		var ms, rowsN, bytes, memory, user, sys, io uint64
		must(rows.Scan(&id, &ms, &rowsN, &bytes, &memory, &user, &sys, &io))
		metrics = append(metrics, map[string]any{"query_id": id, "duration_ms": ms, "read_rows": rowsN, "read_bytes": bytes, "memory_bytes": memory, "cpu_user_us": user, "cpu_system_us": sys, "os_read_bytes": io})
	}
	must(rows.Err())
	rows.Close()
	save(filepath.Join(out, "metrics.json"), metrics)
	fmt.Printf("%s %d repetitions complete\n", db, repeats)
}
