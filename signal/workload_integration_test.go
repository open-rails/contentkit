package signal

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/open-rails/contentkit/contentref"
)

// workload describes a synthetic collection pattern. Every session is one
// selected-version consumption with Checkpoints cumulative revisions; each
// checkpoint is one host request writing the work row, the version row and,
// in the legacy shape, one view row per parent taxonomy record.
type workload struct {
	Sessions, Checkpoints, Parents, Days, Subjects, Works int
	ReactionEvery, ClickEvery                             int
	Workers                                               int
}

type workloadReport struct {
	Requests, Signals             int
	RawEventRows, CanonicalEvents uint64
	Identities                    uint64
	StateRows, DailyRows          uint64
	WriteP50, WriteP95, WriteP99  time.Duration
	Reads                         map[string]time.Duration
}

func runWorkload(t *testing.T, st *Store, conn Conn, tenant string, w workload) workloadReport {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(879))
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	type request struct{ signals []Signal }
	var requests []request
	identities := map[string]struct{}{}
	for s := 0; s < w.Sessions; s++ {
		subject := Subject{UserID: fmt.Sprintf("u%d", rng.Intn(w.Subjects))}
		if s%3 == 0 {
			subject = Subject{AnonKey: fmt.Sprintf("a%d", rng.Intn(w.Subjects))}
		}
		work := strconv.Itoa(rng.Intn(w.Works))
		version := work + "-v" + strconv.Itoa(rng.Intn(2))
		at := start.Add(time.Duration(rng.Intn(w.Days*24*60)) * time.Minute)
		id := fmt.Sprintf("session-%d", s)
		for c := 1; c <= w.Checkpoints; c++ {
			view := Signal{ContentRef: gallery(tenant, work), Subject: subject, Type: TypeView,
				EventID: id, Revision: uint64(c), OccurredAt: at, DurationS: uint32(60 * c), Progress: uint32(3 * c),
				ProgressMax: uint32(3 * w.Checkpoints), Score: int16(10 * c), Completed: c == w.Checkpoints,
				Resume: "p:" + strconv.Itoa(3*c), Payload: map[string]any{"language": "en", "entry_source": "search"}}
			edition := view
			edition.ContentRef = view.WithVersion(version)
			sigs := []Signal{view, edition}
			for p := 0; p < w.Parents; p++ {
				parent := view
				parent.ContentRef = contentref.New(tenant, "tag", fmt.Sprintf("t%d", (len(work)*7+int(work[0])+p)%50))
				parent.EventID = id + ":tag:" + parent.ContentID
				parent.Payload = nil
				sigs = append(sigs, parent)
			}
			if c == w.Checkpoints && w.ReactionEvery > 0 && s%w.ReactionEvery == 0 {
				sigs = append(sigs, Signal{ContentRef: view.ContentRef, Subject: subject, Type: "reaction", EventID: "pref",
					Revision: uint64(s), OccurredAt: at.Add(time.Minute), Value: 1})
			}
			if c == 1 && w.ClickEvery > 0 && s%w.ClickEvery == 0 {
				sigs = append(sigs, Signal{ContentRef: view.ContentRef, Subject: subject, Type: "click",
					EventID: "click:" + id, OccurredAt: at}.WithAttribution(Attribution{RenderID: "r" + id, Surface: SurfaceSearch, Position: 3}))
			}
			for _, sig := range sigs {
				identities[fmt.Sprint(sig.Key(), sig.Subject.Kind(), sig.Subject.Key(), sig.Type, sig.EventID)] = struct{}{}
			}
			requests = append(requests, request{signals: sigs})
		}
	}

	durations := make([]time.Duration, len(requests))
	var (
		wg   sync.WaitGroup
		next = make(chan int)
		fail = make(chan error, w.Workers)
	)
	for i := 0; i < w.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				began := time.Now()
				if err := st.RecordSignals(ctx, tenant, requests[i].signals); err != nil {
					fail <- err
					return
				}
				durations[i] = time.Since(began)
			}
		}()
	}
	for i := range requests {
		next <- i
	}
	close(next)
	wg.Wait()
	close(fail)
	for err := range fail {
		t.Fatal(err)
	}

	rep := workloadReport{Requests: len(requests), Identities: uint64(len(identities)), Reads: map[string]time.Duration{}}
	for _, r := range requests {
		rep.Signals += len(r.signals)
	}
	slices.Sort(durations)
	pct := func(p float64) time.Duration { return durations[int(p*float64(len(durations)-1))] }
	rep.WriteP50, rep.WriteP95, rep.WriteP99 = pct(0.50), pct(0.95), pct(0.99)

	scalar := func(q string, args ...any) uint64 {
		t.Helper()
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var n uint64
		if rows.Next() {
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	rep.RawEventRows = scalar("SELECT count() FROM "+testDB+".signals WHERE tenant = ?", tenant)
	rep.CanonicalEvents = scalar("SELECT count() FROM (SELECT 1 FROM "+testDB+".signals WHERE tenant = ? GROUP BY content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id)", tenant)
	rep.StateRows = scalar("SELECT count() FROM "+testDB+".subject_content_state FINAL WHERE tenant = ?", tenant)
	rep.DailyRows = scalar("SELECT count() FROM "+testDB+".subject_content_daily FINAL WHERE tenant = ?", tenant)

	now := start.AddDate(0, 0, w.Days)
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	timed := func(name string, fn func() error) {
		began := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rep.Reads[name] = time.Since(began)
	}
	for _, n := range []int{7, 30, 90, 365} {
		timed(fmt.Sprintf("Popular %dd", n), func() error {
			_, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: LastDays(n, now), Limit: 50})
			return err
		})
	}
	timed("Popular all", func() error {
		_, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Limit: 50})
		return err
	})
	timed("Metrics 50 ids 30d", func() error {
		_, err := st.Metrics(ctx, tenant, refs(tenant, ids...), LastDays(30, now))
		return err
	})
	timed("States 50 refs", func() error {
		_, err := st.States(ctx, tenant, Subject{UserID: "u1"}, refs(tenant, ids...))
		return err
	})
	timed("History 50", func() error {
		_, err := st.History(ctx, tenant, Subject{UserID: "u1"}, HistoryOptions{Limit: 50})
		return err
	})
	return rep
}

// #879: rows, bytes, amplification and latency of the minimal collection
// shape versus the legacy per-parent fan-out, on the same sessions. The row
// invariants always run; set CONTENTKIT_SIGNAL_WORKLOAD_SESSIONS to measure at
// a larger scale.
func TestIntegrationSyntheticWorkload(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	w := workload{Sessions: 60, Checkpoints: 3, Days: 30, Subjects: 30, Works: 40, ReactionEvery: 10, ClickEvery: 3, Workers: 4}
	if v := os.Getenv("CONTENTKIT_SIGNAL_WORKLOAD_SESSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		w.Sessions, w.Subjects, w.Works, w.Days = n, n/2, n/4, 365
	}
	minimal := w
	legacy := w
	legacy.Parents = 8
	reports := map[string]workloadReport{
		"minimal": runWorkload(t, st, conn, "minimal", minimal),
		"fanout8": runWorkload(t, st, conn, "fanout8", legacy),
	}

	// Canonical rows are the distinct source identities, independent of checkpoints and retries.
	for name, rep := range reports {
		if rep.CanonicalEvents != rep.Identities {
			t.Fatalf("%s canonical events %d, want %d identities", name, rep.CanonicalEvents, rep.Identities)
		}
	}
	all, err := st.Popular(ctx, "minimal", "gallery", PopularOptions{Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var views, completions uint64
	for _, h := range all {
		views += h.Views
		completions += h.Completions
	}
	if views != uint64(w.Sessions) || completions != uint64(w.Sessions) {
		t.Fatalf("checkpoints must count as one completed work view per session, versions apart: views=%d completions=%d", views, completions)
	}

	for _, table := range []string{"signals", "subject_content_state", "subject_content_daily"} {
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+"."+table+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	// Bytes are per database (both tenants); report the split by canonical share.
	rows, err := conn.Query(ctx, `SELECT table, sum(data_compressed_bytes), sum(data_uncompressed_bytes), sum(rows)
FROM system.parts WHERE database = ? AND active GROUP BY table ORDER BY table`, testDB)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var (
			table                  string
			compressed, raw, count uint64
		)
		if err := rows.Scan(&table, &compressed, &raw, &count); err != nil {
			t.Fatal(err)
		}
		t.Logf("after merges %-22s rows=%d compressed=%dB uncompressed=%dB (%.1f B/row)", table, count, compressed, raw, float64(compressed)/float64(max(count, 1)))
	}
	rows.Close()
	for _, name := range []string{"minimal", "fanout8"} {
		rep := reports[name]
		t.Logf("%s: sessions=%d requests=%d signals=%d (%.1f/session) statements=%d raw_rows=%d canonical=%d state_rows=%d daily_rows=%d",
			name, w.Sessions, rep.Requests, rep.Signals, float64(rep.Signals)/float64(w.Sessions), rep.Requests*3,
			rep.RawEventRows, rep.CanonicalEvents, rep.StateRows, rep.DailyRows)
		t.Logf("%s: write p50=%s p95=%s p99=%s workers=%d", name, rep.WriteP50, rep.WriteP95, rep.WriteP99, w.Workers)
		for _, k := range slices.Sorted(func(yield func(string) bool) {
			for k := range rep.Reads {
				if !yield(k) {
					return
				}
			}
		}) {
			t.Logf("%s: read %-20s %s", name, k, rep.Reads[k])
		}
	}
	if os.Getenv("CONTENTKIT_SIGNAL_WORKLOAD_SESSIONS") != "" {
		if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatal(err)
		}
		qrows, err := conn.Query(ctx, `SELECT
    multiIf(query LIKE 'INSERT INTO `+testDB+`.signals%', 'insert signals', query LIKE 'INSERT INTO `+testDB+`.subject_content_state%', 'project state', 'project daily') AS stmt,
    count(), avg(read_rows), avg(written_rows), quantile(0.5)(query_duration_ms), quantile(0.95)(query_duration_ms)
FROM system.query_log
WHERE type = 'QueryFinish' AND event_date >= yesterday()
  AND (query LIKE 'INSERT INTO `+testDB+`.signals%' OR query LIKE 'INSERT INTO `+testDB+`.subject_content_state%' OR query LIKE 'INSERT INTO `+testDB+`.subject_content_daily%')
GROUP BY stmt ORDER BY stmt`)
		if err != nil {
			t.Fatal(err)
		}
		for qrows.Next() {
			var (
				stmt            string
				n               uint64
				readRows, wrote float64
				p50, p95        float64
			)
			if err := qrows.Scan(&stmt, &n, &readRows, &wrote, &p50, &p95); err != nil {
				t.Fatal(err)
			}
			t.Logf("query_log %-14s n=%d avg_read_rows=%.1f avg_written_rows=%.1f p50=%.0fms p95=%.0fms", stmt, n, readRows, wrote, p50, p95)
		}
		qrows.Close()
	}
}

// Analytics outages surface as prompt errors the host can count and drop or
// retry; nothing blocks past the caller's deadline.
func TestIntegrationUnavailableClickHouseFailsFast(t *testing.T) {
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{"127.0.0.1:1"}, DialTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, err := NewStore(conn, "hub")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	began := time.Now()
	err = st.RecordSignals(ctx, "t", []Signal{{ContentRef: gallery("t", "1"), Subject: Subject{UserID: "u"},
		Type: TypeView, EventID: "e", OccurredAt: time.Now()}})
	if err == nil || time.Since(began) > 3*time.Second {
		t.Fatalf("unavailable analytics must fail within the deadline: err=%v after %s", err, time.Since(began))
	}
	if _, err := st.Popular(ctx, "t", "gallery", PopularOptions{}); err == nil {
		t.Fatal("reads must fail too")
	}
	var limit *LimitError
	if err := st.RecordSignals(ctx, "t", make([]Signal, MaxSignalsPerBatch+1)); !errors.As(err, &limit) {
		t.Fatalf("oversize batches are rejected before any network call: %v", err)
	}
}
