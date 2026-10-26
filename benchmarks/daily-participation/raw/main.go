package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-rails/contentkit/signal"
)

type receipt struct {
	Label, QueryID, Kind, SQL string
	WallSeconds               float64
	Error                     string
}
type phase struct {
	Name        string
	WallSeconds float64
	Report      any
}
type conn struct {
	driver.Conn
	out, db, label, runID string
	receipts              []receipt
}

func (c *conn) log(ctx context.Context, q, kind string) (context.Context, func(error)) {
	id := fmt.Sprintf("%s_%s_%04d", c.runID, c.label, len(c.receipts))
	start := time.Now()
	r := receipt{Label: c.label, QueryID: id, Kind: kind, SQL: q}
	return clickhouse.Context(ctx, clickhouse.WithQueryID(id)), func(err error) {
		r.WallSeconds = time.Since(start).Seconds()
		if err != nil {
			r.Error = err.Error()
		}
		c.receipts = append(c.receipts, r)
		save(filepath.Join(c.out, "query-receipts.json"), c.receipts)
	}
}
func (c *conn) Exec(ctx context.Context, q string, args ...any) error {
	ctx, done := c.log(ctx, q, "Exec")
	err := c.Conn.Exec(ctx, q, args...)
	done(err)
	return err
}
func (c *conn) Query(ctx context.Context, q string, args ...any) (driver.Rows, error) {
	ctx, done := c.log(ctx, q, "Query")
	rows, err := c.Conn.Query(ctx, q, args...)
	done(err)
	return rows, err
}
func save(path string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(path, append(b, '\n'), 0600))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func id(prefix uint32, n uint64) string { return fmt.Sprintf("%08x-0000-7000-8000-%012x", prefix, n) }
func main() {
	var output, existingDB, implementation string
	var users int
	flag.StringVar(&output, "output", "", "new output directory")
	flag.IntVar(&users, "users", 60000, "daily users")
	flag.StringVar(&existingDB, "existing-database", "", "retained owned ck261_raw_* database; rebuild only, without inserting or replaying sources")
	flag.StringVar(&implementation, "implementation", "current checkout", "source revision to record")
	flag.Parse()
	const items = 100000
	if output == "" || users < 100 || users > 60000 {
		panic("output required; users100..60000")
	}
	must(os.Mkdir(output, 0700))
	ctx := context.Background()
	addr, user, password := os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR"), os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")
	if addr == "" || user == "" || password == "" {
		panic("owned fixture settings required")
	}
	native, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Username: user, Password: password}, Settings: clickhouse.Settings{"max_threads": 2, "max_insert_threads": 2, "max_memory_usage": 1073741824, "use_query_cache": 0, "log_queries": 1, "log_query_settings": 1}, ReadTimeout: 180 * time.Second})
	must(err)
	defer native.Close()
	db := "ck261_raw_" + time.Now().UTC().Format("20060102_150405")
	if existingDB != "" {
		if !strings.HasPrefix(existingDB, "ck261_raw_") || strings.Trim(existingDB[len("ck261_raw_"):], "0123456789_") != "" {
			panic("owned raw benchmark database required")
		}
		db = existingDB
	}
	c := &conn{Conn: native, db: db, out: output, label: "schema", runID: db + "_" + time.Now().UTC().Format("150405.000000000")}
	if existingDB == "" {
		must(c.Exec(ctx, "CREATE DATABASE "+db))
		// Exact relevant migration definitions, no production migration execution.
		for _, spec := range []struct{ file, table string }{{"0001_baseline.up.sql", "erasures"}, {"0003_measured_engagement.up.sql", "engagement_segments"}, {"0004_measured_population.up.sql", "measured_population_observations"}, {"0006_daily_popularity.up.sql", "popularity_daily"}, {"0006_daily_popularity.up.sql", "popularity_daily_prepared"}} {
			b, e := os.ReadFile(filepath.Join("migrations/clickhouse", spec.file))
			must(e)
			s := string(b)
			start := strings.Index(s, "CREATE TABLE IF NOT EXISTS "+spec.table+" ")
			if start < 0 {
				panic("missing table " + spec.table)
			}
			end := strings.Index(s[start:], ";")
			q := s[start : start+end+1]
			q = strings.Replace(q, "CREATE TABLE IF NOT EXISTS "+spec.table, "CREATE TABLE IF NOT EXISTS "+db+"."+spec.table, 1)
			q = strings.ReplaceAll(q, "{{ON_CLUSTER}}", "")
			must(c.Exec(ctx, q))
		}
	}
	day := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC)
	tenant := "benchmark261"
	kind := "gallery"
	// Five hot items from1k and five cold items from99k, distinct per person.
	itemExpr := "if(slot<5,(person*5+slot)%1000,1000+(person*5+slot-5)%99000)"
	uuidSQL := func(prefix, expr string) string {
		return "concat('" + prefix + "-0000-7000-8000-',leftPad(lower(hex(" + expr + ")),12,'0'))"
	}
	pairs := users * 10
	canonical := fmt.Sprintf(`WITH toUInt64(intDiv(number,6)) AS pair, toUInt64(intDiv(pair,10)) AS person,
 toUInt64(pair%%10) AS slot, toUInt64(intDiv(number%%6,2)) AS receipt, toUInt64(2-number%%2) AS rev,
 %s AS item
 SELECT '%s','%s',%s,%s,'user',%s,%s,rev,%s,%s,
 toDateTime64('2025-01-15 12:00:00',6,'UTC'),
 toUInt32(if(person%%10=0,100,if(rev=2,400,200))),[toUInt32(0)],map(toUInt32(0),toUInt64(if(person%%10=0,100,if(rev=2,400,200)))),toUInt32(100),toUInt32(100),'p0',
 '{"observed_at":"2025-01-15T12:00:00Z","content_access":"public","viewer_access":"full","policy_revision":"bench261","audience":"public","eligibility_known":true,"domain":"main","language":"en"}',
 '{"pages":[{"index":0,"ranges":[{"from":0,"to":100}],"active_ms":400}]}'
 FROM numbers(%d)`, itemExpr, tenant, kind, uuidSQL("00000001", "item"), uuidSQL("00000002", "item"), uuidSQL("00000003", "person"), uuidSQL("00000004", "pair*3+receipt"), uuidSQL("00000005", "pair"), uuidSQL("00000006", "pair*3+receipt"), pairs*6)
	insert := `INSERT INTO ` + db + `.engagement_segments (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement) ` + canonical
	insertSource := func(label, statement string) {
		for offset := 0; offset < pairs*6; offset += 300000 {
			n := min(300000, pairs*6-offset)
			c.label = fmt.Sprintf("%s_%d", label, offset/300000)
			must(c.Exec(ctx, strings.Replace(statement, fmt.Sprintf("FROM numbers(%d)", pairs*6), fmt.Sprintf("FROM numbers(%d,%d)", offset, n), 1)))
		}
	}
	if existingDB == "" {
		insertSource("insert_source", insert)
	}
	population := fmt.Sprintf(`INSERT INTO %s.measured_population_observations
 (tenant,subject_kind,subject,content_kind,observation_id,observed_at,domain,language,eligible_tiers,eligibility_known)
 WITH toUInt64(intDiv(number,4)) AS person,toUInt64(intDiv(number%%4,2)) AS observation
 SELECT '%s','user',%s,'%s',%s,toDateTime64('2025-01-15 11:00:00',6,'UTC')+toIntervalSecond(observation),
 'main','en',if(observation=1 AND person%%2=0,['gold'],CAST([],'Array(String)')),true FROM numbers(%d)`, db, tenant, uuidSQL("00000003", "person"), kind, uuidSQL("00000007", "person*2+observation"), users*4)
	c.label = "insert_population"
	if existingDB == "" {
		must(c.Exec(ctx, population))
	}
	expected := make([]uint64, items)
	eligible := make([]uint64, items)
	for item := 0; item < items; item++ {
		if item%2 == 0 {
			eligible[item] = uint64(users)
		} else {
			eligible[item] = uint64((users + 1) / 2)
		}
	}
	for person := 0; person < users; person++ {
		for slot := 0; slot < 10; slot++ {
			item := (person*5 + slot) % 1000
			if slot >= 5 {
				item = 1000 + (person*5+slot-5)%99000
			}
			if person%10 != 0 && (item%2 == 0 || person%2 == 0) {
				expected[item]++
			}
		}
	}
	availability := signal.Availability{Known: true, From: day, Until: day.AddDate(0, 0, 1)}
	for item := 0; item < items; item++ {
		a := signal.AccessInterval{ID: fmt.Sprint(item), ContentID: id(1, uint64(item)), From: day, Until: day.AddDate(0, 0, 1), Audience: "public"}
		if item%2 != 0 {
			a.Audience = "tier"
			a.AudienceTiers = []string{"gold"}
		}
		availability.Intervals = append(availability.Intervals, a)
	}
	store, err := signal.NewStore(c, db)
	must(err)
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	var phases []phase
	prep := func(label string, rebuild bool) {
		c.label = label
		start := time.Now()
		r, e := store.PrepareDailyPopularity(ctx, tenant, kind, availability, scope, rebuild)
		phases = append(phases, phase{label, time.Since(start).Seconds(), r})
		save(filepath.Join(output, "phases.json"), phases)
		must(e)
	}
	verify := func(label string) {
		c.label = label
		rows, e := c.Query(ctx, `SELECT content_id,readers,eligible FROM `+db+`.popularity_daily FINAL ORDER BY content_id`)
		must(e)
		n := 0
		var total uint64
		for rows.Next() {
			var item string
			var u, pop uint64
			must(rows.Scan(&item, &u, &pop))
			if n >= items || item != id(1, uint64(n)) || u != expected[n] || pop != eligible[n] {
				panic(fmt.Sprintf("count mismatch item%d id%s U%d/%d N%d/%d", n, item, u, expected[n], pop, eligible[n]))
			}
			total += u
			n++
		}
		must(rows.Err())
		rows.Close()
		if n != items {
			panic("lost daily items")
		}
		save(filepath.Join(output, label+".json"), map[string]any{"items_verified": n, "qualified_item_people": total, "population_public": users, "population_gold": (users + 1) / 2})
	}
	if existingDB == "" {
		prep("prepare_initial", false)
		verify("verify_initial")
		prep("reuse_closed_day", false)
		// Replay identical identities in opposite revision order, never new receipts.
		insertSource("replay_source", strings.Replace(insert, "toUInt64(2-number%2) AS rev", "toUInt64(1+number%2) AS rev", 1))
		c.label = "replay_population"
		must(c.Exec(ctx, population))
		prep("reuse_after_replay", false)
		verify("verify_reused")
		prep("explicit_rebuild_after_replay", true)
		verify("verify_rebuilt")
	} else {
		verify("verify_existing_before")
		prep("existing_explicit_rebuild", true)
		verify("verify_existing_rebuilt")
		prep("existing_reuse_finalized", false)
	}
	c.label = "rank"
	start := time.Now()
	hits, err := store.DailyPopular(ctx, tenant, kind, signal.Between(day, day.AddDate(0, 0, 1)), scope, items)
	must(err)
	phases = append(phases, phase{"rank", time.Since(start).Seconds(), map[string]int{"results": len(hits)}})
	want := []int{}
	for item, n := range expected {
		if n > 0 {
			want = append(want, item)
		}
	}
	sort.Slice(want, func(i, j int) bool {
		a, b := want[i], want[j]
		sa, sb := float64(expected[a])/float64(eligible[a]+20), float64(expected[b])/float64(eligible[b]+20)
		if sa != sb {
			return sa > sb
		}
		return a < b
	})
	if len(hits) != len(want) {
		panic("rank lost nonzero participants")
	}
	for i, hit := range hits {
		n := want[i]
		if hit.ContentID != id(1, uint64(n)) || math.Abs(hit.Score-float64(expected[n])/float64(eligible[n]+20)) > 1e-12 {
			panic("rank differs from expected counts")
		}
	}
	save(filepath.Join(output, "phases.json"), phases)
	c.label = "flush_logs"
	must(c.Exec(ctx, "SYSTEM FLUSH LOGS"))
	ids := make([]string, len(c.receipts))
	for i, r := range c.receipts {
		ids[i] = r.QueryID
	}
	rows, err := native.Query(ctx, `SELECT query_id,query_duration_ms,read_rows,read_bytes,written_rows,written_bytes,memory_usage,
 ProfileEvents['UserTimeMicroseconds'],ProfileEvents['SystemTimeMicroseconds'],ProfileEvents['OSReadBytes'],exception_code
 FROM system.query_log WHERE query_id IN ? AND type='QueryFinish'`, ids)
	must(err)
	var metrics []map[string]any
	for rows.Next() {
		var q string
		var duration, read, bytes, written, wbytes, memory uint64
		var cpu, sys, io uint64
		var ex int32
		must(rows.Scan(&q, &duration, &read, &bytes, &written, &wbytes, &memory, &cpu, &sys, &io, &ex))
		metrics = append(metrics, map[string]any{"query_id": q, "duration_ms": duration, "read_rows": read, "read_bytes": bytes, "written_rows": written, "written_bytes": wbytes, "memory_bytes": memory, "cpu_user_us": cpu, "cpu_system_us": sys, "os_read_bytes": io, "exception_code": ex})
	}
	must(rows.Err())
	rows.Close()
	save(filepath.Join(output, "query-metrics.json"), metrics)
	save(filepath.Join(output, "metadata.json"), map[string]any{"database": db, "users": users, "items": items, "person_item_days": pairs, "initial_source_rows": pairs * 6, "canonical_receipts": pairs * 3, "initial_population_rows": users * 4, "source_deliveries": "revision2 then1; replay1 then2", "scope": scope, "implementation": implementation, "existing_database_rebuild_only": existingDB != "", "historical_day": day, "limits": "Synthetic direct table inserts; actual PrepareDailyPopularity/DailyPopular execute; no HTTP ingestion timing or year raw dataset"})
	fmt.Println(db)
	for _, p := range phases {
		fmt.Printf("%s %.3fs %+v\n", p.Name, p.WallSeconds, p.Report)
	}
}
