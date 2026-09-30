package signal

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
