package signal

import (
	"fmt"
	"testing"
)

// Minimized form of normalized ranking: a measured per-person relation used
// on the right side of a join, with and without an upstream interval join.
func TestMeasuredPersonRelationCanBeJoined(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	tenant := "scope-probe"
	event := readingFixture{ContentRef: gallery(tenant, cid(710)), Subject: Subject{UserID: "reader"}, Type: TypeView, EventID: "page", OccurredAt: at(1, 10), Progress: 1, ProgressMax: 1}.native()
	if err := st.RecordSignals(ctx, tenant, []Signal{event}); err != nil {
		t.Fatal(err)
	}
	for _, intervalJoin := range []bool{false, true} {
		t.Run(fmt.Sprint(intervalJoin), func(t *testing.T) {
			canonical := st.measuredCanonical("1")
			prefix := ""
			if intervalJoin {
				prefix = "intervals AS (SELECT 'gallery' AS interval_id,'" + event.ContentID + "' AS content_id),receipts AS (" + canonical + "),"
				canonical = "SELECT i.interval_id AS content_kind,c.content_id,c.content_version_id,c.subject_kind,c.subject,c.receipt_id,c.c,c.ver FROM intervals i INNER JOIN receipts c ON i.content_id=c.content_id"
			}
			query := `WITH ` + prefix + `versions AS (` + measuredVersions(canonical) + ` SELECT content_kind AS interval_id,subject_kind,subject,qualified,attention_quality FROM scored),
 people AS (SELECT interval_id,subject_kind,subject,max(attention_quality) AS quality FROM versions WHERE qualified GROUP BY interval_id,subject_kind,subject)
 SELECT count(),sum(p.quality) FROM (SELECT 'gallery' AS interval_id) i LEFT JOIN people p ON p.interval_id=i.interval_id`
			rows, err := conn.Query(ctx, query, tenant)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var count uint64
			var quality float64
			if !rows.Next() {
				t.Fatal("missing aggregate row")
			}
			if err := rows.Scan(&count, &quality); err != nil {
				t.Fatal(err)
			}
			if count != 1 || quality != 0.125 {
				t.Fatalf("count=%d quality=%g", count, quality)
			}
		})
	}
}
