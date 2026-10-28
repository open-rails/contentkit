package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/open-rails/contentkit/signal"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func save(path string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(path, append(b, '\n'), 0600))
}

type result struct {
	Label, QueryID string
	WallSeconds    float64
	Rows           int
	Error          string
}

func main() {
	var out string
	var days int
	flag.StringVar(&out, "output", "", "new output directory")
	flag.IntVar(&days, "days", 365, "days")
	flag.Parse()
	if out == "" || days < 30 || days > 365 {
		panic("output and days30..365")
	}
	must(os.Mkdir(out, 0700))
	ctx := context.Background()
	db := "ck261_actual_" + time.Now().UTC().Format("20060102_150405")
	ch, e := clickhouse.Open(&clickhouse.Options{Addr: []string{os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR")}, Auth: clickhouse.Auth{Username: os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), Password: os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")}, Settings: clickhouse.Settings{"max_threads": 2, "max_insert_threads": 2, "max_memory_usage": 1073741824, "use_query_cache": 0, "log_queries": 1, "log_query_settings": 1}, ReadTimeout: 180 * time.Second})
	must(e)
	defer ch.Close()
	var receipts []result
	timed := func(label string, fn func(context.Context) (int, error)) {
		id := db + "_" + label
		start := time.Now()
		rows, err := fn(clickhouse.Context(ctx, clickhouse.WithQueryID(id)))
		r := result{label, id, time.Since(start).Seconds(), rows, ""}
		if err != nil {
			r.Error = err.Error()
		}
		receipts = append(receipts, r)
		save(filepath.Join(out, "receipts.json"), receipts)
		must(err)
	}
	exec := func(label, query string) {
		timed(label, func(ctx context.Context) (int, error) { return 0, ch.Exec(ctx, query) })
		must(os.WriteFile(filepath.Join(out, label+".sql"), []byte(query+"\n"), 0600))
	}
	exec("database", "CREATE DATABASE "+db)
	b, e := os.ReadFile("migrations/clickhouse/0001_baseline.up.sql")
	must(e)
	s := string(b)
	start := strings.Index(s, "CREATE TABLE IF NOT EXISTS popularity_daily ")
	end := strings.Index(s[start:], ";")
	ddl := strings.ReplaceAll(s[start:start+end+1], "{{ON_CLUSTER}}", "")
	ddl = strings.Replace(ddl, "CREATE TABLE IF NOT EXISTS popularity_daily", "CREATE TABLE IF NOT EXISTS "+db+".popularity_daily", 1)
	exec("table", ddl)
	until := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	from := until.AddDate(0, 0, -days)
	insert := func(label string, nDays int, dayOffset int, version int, extra int) {
		exec(label, fmt.Sprintf(`INSERT INTO %s.popularity_daily
 WITH toUInt32(number%%100000) AS item,toUInt32(intDiv(number,100000)) AS d,
 (item+intDiv(d+%d,7)*137)%%100000 AS rank,
 multiIf(rank<100,1200,rank<1000,200,rank<10000,20,rank<40000,2,1) AS u
 SELECT 'bench261','gallery','main','en',addDays(toDate('%s'),d+%d),
 concat('00000001-0000-7000-8000-',leftPad(lower(hex(item)),12,'0')),
 toUInt64(u+%d),toUInt64(60000),toUInt64(%d) FROM numbers(%d)`, db, dayOffset, from.Format(time.DateOnly), dayOffset, extra, version, nDays*100000))
	}
	// Insert in bounded week batches, preserving normal replica merge behavior.
	for d := 0; d < days; d += 7 {
		insert(fmt.Sprintf("base_%03d", d), min(7, days-d), d, 1, 0)
	}
	// Hold only this newly owned table's merges to guarantee current-day revision
	// overlap during FINAL measurement; no shared table/cache/service is touched.
	exec("hold_owned_merges", "SYSTEM STOP MERGES "+db+".popularity_daily")
	defer func() { must(ch.Exec(context.Background(), "SYSTEM START MERGES "+db+".popularity_daily")) }()
	insert("replace_today", 1, days-1, 2, 5)
	check := func(label string) {
		rows, err := ch.Query(ctx, "SELECT count(),sum(readers) FROM "+db+".popularity_daily FINAL")
		must(err)
		defer rows.Close()
		var n, u uint64
		if !rows.Next() {
			panic("missing aggregate")
		}
		must(rows.Scan(&n, &u))
		if n != uint64(days*100000) || u != uint64(days*600000+500000) {
			panic(fmt.Sprintf("replacement dedupe wrong rows%d counts%d", n, u))
		}
		save(filepath.Join(out, label+".json"), map[string]any{"canonical_rows": n, "sum_readers": u})
	}
	check("final_truth")
	rows, e := ch.Query(ctx, "SELECT sum(rows),count(),sum(bytes_on_disk) FROM system.parts WHERE database=? AND table='popularity_daily' AND active", db)
	must(e)
	var physical, parts, size uint64
	rows.Next()
	must(rows.Scan(&physical, &parts, &size))
	rows.Close()
	if physical <= uint64(days*100000) {
		panic("measurement has no physical replacement overlap")
	}
	store, e := signalstore.NewStore(ch, db)
	must(e)
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	type win struct {
		name string
		days int
	}
	windows := []win{{"7d", 7}, {"30d", 30}, {"year", days}, {"all", days}}
	idsByWindow := map[string][]string{}
	for rep := 1; rep <= 3; rep++ {
		for _, w := range windows {
			window := signalstore.Between(until.AddDate(0, 0, -w.days), until)
			if w.name == "all" {
				window = signalstore.AllTime()
			}
			label := fmt.Sprintf("%s_run%d", w.name, rep)
			timed(label, func(ctx context.Context) (int, error) {
				hits, err := store.DailyPopular(ctx, "bench261", "gallery", window, scope, 100000)
				if err != nil {
					return len(hits), err
				}
				if len(hits) != 100000 {
					panic("lost item IDs")
				}
				ids := make([]string, len(hits))
				for i, h := range hits {
					ids[i] = h.ContentID
					if i > 0 && (hits[i-1].Score < h.Score || hits[i-1].Score == h.Score && hits[i-1].ContentID > h.ContentID) {
						panic("ordering")
					}
				}
				save(filepath.Join(out, label+"-ordered.json"), hits)
				if prior := idsByWindow[w.name]; prior != nil && !slices.Equal(prior, ids) {
					var changed []map[string]any
					for position, before := range prior {
						if before != ids[position] {
							changed = append(changed, map[string]any{"position": position, "previous": before, "current": ids[position]})
						}
					}
					save(filepath.Join(out, label+"-order-changes.json"), changed)
				}
				idsByWindow[w.name] = ids
				for _, item := range []int{0, 99, 100, 999, 1000, 50000, 99999} {
					expected := 0.
					for d := days - w.days; d < days; d++ {
						rank := (item + (d/7)*137) % 100000
						u := 1
						if rank < 100 {
							u = 1200
						} else if rank < 1000 {
							u = 200
						} else if rank < 10000 {
							u = 20
						} else if rank < 40000 {
							u = 2
						}
						if d == days-1 {
							u += 5
						}
						expected += float64(u) / 60020
					}
					expected /= float64(w.days)
					key := fmt.Sprintf("00000001-0000-7000-8000-%012x", item)
					index := slices.Index(ids, key)
					if index < 0 || math.Abs(hits[index].Score-expected) > 1e-12 {
						panic("actual score failed independent oracle")
					}
				}
				if rep == 3 {
					save(filepath.Join(out, w.name+"-ordered.json"), hits)
				}
				return len(hits), nil
			})
		}
	}
	exec("resume_owned_merges", "SYSTEM START MERGES "+db+".popularity_daily")
	exec("flush_logs", "SYSTEM FLUSH LOGS")
	queryIDs := make([]string, len(receipts))
	for i, r := range receipts {
		queryIDs[i] = r.QueryID
	}
	rows, e = ch.Query(ctx, `SELECT query_id,query_duration_ms,read_rows,read_bytes,result_rows,result_bytes,memory_usage,ProfileEvents['UserTimeMicroseconds'],ProfileEvents['SystemTimeMicroseconds'],ProfileEvents['OSReadBytes'],Settings['max_threads'] FROM system.query_log WHERE query_id IN ? AND type='QueryFinish'`, queryIDs)
	must(e)
	var metrics []map[string]any
	for rows.Next() {
		var id, threads string
		var ms, read, bytes, resultRows, resultBytes, memory, cpu, sys, io uint64
		must(rows.Scan(&id, &ms, &read, &bytes, &resultRows, &resultBytes, &memory, &cpu, &sys, &io, &threads))
		metrics = append(metrics, map[string]any{"query_id": id, "duration_ms": ms, "read_rows": read, "read_bytes": bytes, "result_rows": resultRows, "result_bytes": resultBytes, "peak_memory_bytes": memory, "cpu_user_us": cpu, "cpu_system_us": sys, "os_read_bytes": io, "threads": threads})
	}
	must(rows.Err())
	rows.Close()
	save(filepath.Join(out, "metrics.json"), metrics)
	save(filepath.Join(out, "metadata.json"), map[string]any{"database": db, "days": days, "items": 100000, "from": from, "until": until, "physical_rows_before_queries": physical, "parts_before_queries": parts, "bytes_on_disk_before_queries": size, "method": "actual signal.Store.DailyPopular compiled from current checkout; record git rev-parse HEAD with output", "schema": "exact ReplicatedReplacingMergeTree popularity_daily migration0006; String UUID IDs; UInt64 counts/version", "merges": "stopped only owned new table during replacement/FINAL sensitivity and resumed afterward", "proof": "100000 IDs sorted; repeated ID order differences recorded separately; seven sampled scores independently match latest today version"})
	fmt.Println(db)
	for _, r := range receipts {
		if strings.Contains(r.Label, "_run") {
			fmt.Printf("%s %.3fs %d IDs\n", r.Label, r.WallSeconds, r.Rows)
		}
	}
}
