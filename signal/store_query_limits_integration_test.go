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

// The page JSON is bound, never inlined: a {…:…} literal makes clickhouse-go
// treat the statement as using server-side parameters and reject positional args.
const seededMeasurement = `{"pages":[{"index":0,"active_ms":1000,"ranges":[{"from":0,"to":10000}]}]}`

func seedMeasured(ctx context.Context, conn driver.Conn, st *Store, rows string, args ...any) error {
	q := `INSERT INTO ` + st.db + `.engagement_segments
 (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,occurred_at,
 active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement)
 SELECT tenant,'gallery',content_id,version_id,'user',subject,event_id,toUInt64(1),event_id,event_id,at,
 toUInt32(1000),CAST(range(100),'Array(UInt32)'),map(toUInt32(0),toUInt64(1000)),toUInt32(100),toUInt32(100),'',
 toJSONString(map('observed_at',formatDateTime(at,'%Y-%m-%dT%H:%i:%SZ'),'content_access','public','viewer_access','full','audience','public')),
 ? FROM (` + rows + `)`
	return conn.Exec(ctx, q, append([]any{seededMeasurement}, args...)...)
}

func TestIntegrationRepairDailyPartitionLimit(t *testing.T) {
	st, conn := queryLimitStore(t)
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{"max_partitions_per_insert_block": 100, "max_memory_usage": 536870912, "max_threads": 1}))
	ref := gallery("repair", cid(1))
	subject := Subject{UserID: "history"}
	seed := `SELECT 'repair' AS tenant,? AS content_id,'fixture-edition' AS version_id,? AS subject,
 toString(number+?) AS event_id,addMonths(toDateTime64('2010-01-01',6,'UTC'),toInt32(number+?)) AS at FROM numbers(55)`
	for start := 0; start < 165; start += 55 {
		if err := seedMeasured(ctx, conn, st, seed, ref.ContentID, subject.Key(), start, start); err != nil {
			t.Fatal(err)
		}
	}
	bulk := `SELECT 'repair' AS tenant,? AS content_id,'fixture-edition' AS version_id,? AS subject,
 concat('bulk-',toString(number)) AS event_id,toDateTime64('2015-03-01',6,'UTC') AS at FROM numbers(100000)`
	if err := seedMeasured(ctx, conn, st, bulk, ref.ContentID, subject.Key()); err != nil {
		t.Fatal(err)
	}
	orphan := `INSERT INTO ` + st.db + `.engagement_daily
 (tenant,content_kind,content_id,content_version_id,subject_kind,subject,day,present,active_ms,covered_units,unit_attention,unit_total,unit_scale,version)
 VALUES('repair','gallery',?,'fixture-edition','user',?,'1980-01-01',true,1000,CAST(range(100),'Array(UInt32)'),map(toUInt32(0),toUInt64(1000)),100,100,1)`
	if err := conn.Exec(ctx, orphan, ref.ContentID, subject.Key()); err != nil {
		t.Fatal(err)
	}
	gate := newGate(conn, "INSERT INTO "+st.db+".engagement_daily")
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
		t.Fatalf("interrupted rebuild: %+v %v", partial, err)
	}
	var partialDays uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+st.db+".engagement_daily FINAL WHERE tenant='repair'").Scan(&partialDays); err != nil || partialDays != 100 {
		t.Fatalf("first bounded batch missing: %d %v", partialDays, err)
	}
	result, err := st.RepairProjections(ctx, "repair", RepairOptions{})
	if err != nil || result.Examined != 1 || result.Repaired != 1 || result.Next != nil {
		t.Fatalf("165-month rebuild: %+v %v", result, err)
	}
	assertDaily := func(wantZero uint64) {
		t.Helper()
		q := `SELECT countIf(present),countIf(NOT present),sum(active_ms),
 countIf(present AND version!=(SELECT max(version) FROM ` + st.db + `.engagement_state WHERE tenant='repair' AND content_version_id='fixture-edition'))
 FROM ` + st.db + `.engagement_daily FINAL WHERE tenant='repair'`
		var positive, zero, active, wrong uint64
		if err := conn.QueryRow(ctx, q).Scan(&positive, &zero, &active, &wrong); err != nil {
			t.Fatal(err)
		}
		if positive != 165 || zero != wantZero || active != 100165000 || wrong != 0 {
			t.Fatalf("daily coverage: positive=%d zero=%d active=%d wrongGeneration=%d", positive, zero, active, wrong)
		}
	}
	assertDaily(1)
	moved := readingFixture{ContentRef: ref, Subject: subject, Type: TypeView, EventID: "0", Revision: 2, OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Progress: 1, ProgressMax: 1}
	for range 2 {
		if err := recordFixtureSignals(st, ctx, "repair", []readingFixture{moved}); err != nil {
			t.Fatal(err)
		}
		assertDaily(2)
	}
	clean, err := st.RepairProjections(ctx, "repair", RepairOptions{})
	if err != nil || clean.Examined != 1 || clean.Repaired != 0 {
		t.Fatalf("replay not converged: %+v %v", clean, err)
	}
}

func TestIntegrationRepairDefaultPageQueryLimit(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%t", rebuild), func(t *testing.T) {
			st, conn := queryLimitStore(t)
			ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}))
			version, subject := cid(20000), cid(30000)
			q := `SELECT 'repair' AS tenant,concat('01920000-0000-7000-8000-',leftPad(toString(number+1),12,'0')) AS content_id,
 ? AS version_id,? AS subject,toString(number) AS event_id,toDateTime64('2026-05-01',6,'UTC') AS at FROM numbers(1001)`
			if err := seedMeasured(ctx, conn, st, q, version, subject); err != nil {
				t.Fatal(err)
			}
			first, err := st.RepairProjections(ctx, "repair", RepairOptions{Rebuild: rebuild})
			if err != nil || first.Examined != 1000 || first.Repaired != 1000 || first.Next == nil || first.Next.ContentID != cid(1000) {
				t.Fatalf("first page: %+v %v", first, err)
			}
			second, err := st.RepairProjections(ctx, "repair", RepairOptions{Rebuild: rebuild, After: first.Next})
			if err != nil || second.Examined != 1 || second.Repaired != 1 || second.Next != nil {
				t.Fatalf("second page: %+v %v", second, err)
			}
			clean, err := st.RepairProjections(ctx, "repair", RepairOptions{})
			if err != nil || clean.Examined != 1000 || clean.Repaired != 0 || clean.Next == nil {
				t.Fatalf("clean pagination: %+v %v", clean, err)
			}
			refs := make([]ContentRef, 1001)
			for i := range refs {
				refs[i] = gallery("repair", cid(i+1)).WithVersion(version)
			}
			metrics, err := st.Metrics(ctx, "repair", refs, AllTime())
			if err != nil || len(metrics) != len(refs) {
				t.Fatalf("repaired metrics missing: %d %v", len(metrics), err)
			}
			for _, ref := range refs {
				if metrics[ref.Key()].Readers != 1 {
					t.Fatalf("measured reader missing for %+v", ref)
				}
			}
		})
	}
}
