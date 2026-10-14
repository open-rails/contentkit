package signal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
)

func queryLimitStore(t *testing.T) (*Store, driver.Conn) {
	t.Helper()
	e := signaltest.FromEnv(t)
	db := "ck_query_limits_" + strings.ReplaceAll(contentref.NewID(), "-", "")
	conn := e.Empty(t, db)
	t.Cleanup(func() { e.Drop(t, conn, db) })
	e.Apply(t, conn)
	st, err := NewStore(conn, db)
	if err != nil {
		t.Fatal(err)
	}
	return st, conn
}

func TestIntegrationRepairDailyPartitionLimit(t *testing.T) {
	st, conn := queryLimitStore(t)
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{
		"max_partitions_per_insert_block": 100,
		"max_memory_usage":                536870912,
		"max_threads":                     1,
	}))
	ref := gallery("repair", cid(1))
	subject := Subject{UserID: "history"}
	// One key can span more partitions than the server permits in an INSERT.
	// Seed in three bounded inserts; projection calls keep the default limit.
	seed := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, revision, occurred_at)
SELECT ?, 'gallery', ?, 'user', ?, 'view', toString(number + ?), 1,
       addMonths(toDateTime('2010-01-01'), toInt32(number + ?))
FROM numbers(55)`, st.db)
	for start := 0; start < 165; start += 55 {
		if err := conn.Exec(ctx, seed, "repair", ref.ContentID, subject.Key(), start, start); err != nil {
			t.Fatal(err)
		}
	}
	bulk := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, subject_kind, subject, signal_type, event_id, revision, occurred_at)
SELECT 'repair', 'gallery', ?, 'user', ?, 'view', concat('bulk-', toString(number)), 1, toDateTime('2015-03-01')
FROM numbers(100000)`, st.db)
	if err := conn.Exec(ctx, bulk, ref.ContentID, subject.Key()); err != nil {
		t.Fatal(err)
	}
	// This positive orphan month is absent even from the raw history.
	orphan := fmt.Sprintf(`INSERT INTO %s.subject_content_daily
(tenant, content_kind, content_id, subject_kind, subject, day, events, views, version)
VALUES ('repair', 'gallery', ?, 'user', ?, '1980-01-01', 1, 1, 1)`, st.db)
	if err := conn.Exec(ctx, orphan, ref.ContentID, subject.Key()); err != nil {
		t.Fatal(err)
	}

	gate := newGate(conn, "INSERT INTO "+st.db+".subject_content_daily")
	close(gate.release)
	interruption := errors.New("second daily batch interrupted")
	batches := 0
	gate.dispatch = func(ctx context.Context, q string, args ...any) error {
		batches++
		if batches == 2 {
			return interruption
		}
		return conn.Exec(ctx, q, args...)
	}
	writer := *st
	writer.conn = gate
	partial, err := writer.RepairProjections(ctx, "repair", RepairOptions{Rebuild: true})
	if !errors.Is(err, interruption) || partial.Repaired != 0 {
		t.Fatalf("interrupted rebuild: %+v, %v", partial, err)
	}
	var partialDays uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+st.db+".subject_content_daily FINAL WHERE tenant = 'repair'").Scan(&partialDays); err != nil || partialDays != 100 {
		t.Fatalf("first batch must be durable before failure: days=%d, %v", partialDays, err)
	}
	result, err := st.RepairProjections(ctx, "repair", RepairOptions{})
	if err != nil || result.Examined != 1 || result.Repaired != 1 || result.Next != nil {
		t.Fatalf("165-month rebuild: %+v, %v", result, err)
	}
	assertDaily := func(wantZeroDays uint64) {
		t.Helper()
		q := fmt.Sprintf(`SELECT countIf(events > 0), countIf(events = 0), sum(events), sum(views),
    countIf(events > 0 AND version != (SELECT max(version) FROM %s.subject_content_state WHERE tenant = 'repair'))
FROM %s.subject_content_daily FINAL WHERE tenant = 'repair'`, st.db, st.db)
		var positive, zero, events, views, wrongGeneration uint64
		if err := conn.QueryRow(ctx, q).Scan(&positive, &zero, &events, &views, &wrongGeneration); err != nil {
			t.Fatal(err)
		}
		if positive != 165 || zero != wantZeroDays || events != 100165 || views != 100165 || wrongGeneration != 0 {
			t.Fatalf("daily rows: positive=%d zero=%d events=%d views=%d wrongGeneration=%d", positive, zero, events, views, wrongGeneration)
		}
	}
	assertDaily(1)
	// Moving the first event into a new month must clear its old day without
	// reducing any batch's generation to just that batch's events.
	moved := Signal{ContentRef: ref, Subject: subject, Type: TypeView, EventID: "0", Revision: 2,
		OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for i := 0; i < 2; i++ {
		if err := st.RecordSignals(ctx, "repair", []Signal{moved}); err != nil {
			t.Fatal(err)
		}
		assertDaily(2)
	}
	clean, err := st.RepairProjections(ctx, "repair", RepairOptions{})
	if err != nil || clean.Examined != 1 || clean.Repaired != 0 {
		t.Fatalf("revised/replayed projections must be current: %+v, %v", clean, err)
	}
}

func TestIntegrationRepairDefaultPageQueryLimit(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%t", rebuild), func(t *testing.T) {
			st, conn := queryLimitStore(t)
			ctx := clickhouse.Context(context.Background(),
				clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}),
			)
			version, subject := cid(20000), cid(30000)
			// Raw events without projections model an interrupted writer.
			q := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id, occurred_at)
SELECT ?, 'gallery', concat(?, leftPad(toString(number + 1), 12, '0')), ?, 'user', ?, 'view', toString(number), ?
FROM numbers(1001)`, st.db)
			for _, tenant := range []string{"repair", "other"} {
				if err := conn.Exec(ctx, q, tenant, testIDPrefix, version, subject, at(1, 10)); err != nil {
					t.Fatal(err)
				}
			}
			first, err := st.RepairProjections(ctx, "repair", RepairOptions{Rebuild: rebuild})
			if err != nil {
				t.Fatal(err)
			}
			if first.Examined != 1000 || first.Repaired != 1000 || first.Next == nil || first.Next.ContentID != cid(1000) {
				t.Fatalf("first page: %+v", first)
			}
			last, err := st.RepairProjections(ctx, "repair", RepairOptions{Rebuild: rebuild, After: first.Next})
			if err != nil || last.Examined != 1 || last.Repaired != 1 || last.Next != nil {
				t.Fatalf("last page: %+v, %v", last, err)
			}
			clean, err := st.RepairProjections(ctx, "repair", RepairOptions{})
			if err != nil || clean.Examined != 1000 || clean.Repaired != 0 || clean.Next == nil || clean.Next.ContentID != cid(1000) {
				t.Fatalf("fresh projections must retain candidate pagination: %+v, %v", clean, err)
			}
			refs := make([]ContentRef, 1001)
			for i := range refs {
				refs[i] = gallery("repair", cid(i+1)).WithVersion(version)
			}
			metrics, err := st.Metrics(ctx, "repair", refs, AllTime())
			if err != nil || len(metrics) != len(refs) {
				t.Fatalf("repaired metrics count=%d, want %d: %v", len(metrics), len(refs), err)
			}
			for _, ref := range refs {
				if metrics[ref.Key()].Views != 1 {
					t.Fatalf("missing or cross-tenant projection for %s: %+v", ref, metrics[ref.Key()])
				}
			}
		})
	}
}
