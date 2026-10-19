package signal

import (
	"context"
	"testing"
	"time"
)

func TestIntegrationReadingRecencyRepairRestoresHistory(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	ref := gallery("doujins", cid(1))
	sub := Subject{UserID: "u1"}
	when := func(day int) time.Time { return time.Date(2026, 5, day, 10, 0, 0, 0, time.UTC) }
	first := readingFixture{ContentRef: ref, Subject: sub, Type: TypeView, EventID: "first", OccurredAt: when(2), Progress: 2, ProgressMax: 20, Resume: "p:2"}
	if err := recordFixtureSignals(st, ctx, "doujins", []readingFixture{first}); err != nil {
		t.Fatal(err)
	}
	// The durable source was written, but projection crashed before the newer
	// reading position was visible. A later click is not a reading checkpoint.
	later := first
	later.EventID, later.OccurredAt, later.Progress, later.Resume = "later", when(4), 4, "p:4"
	if err := insertFixtureSegment(ctx, conn, "doujins", later.native()); err != nil {
		t.Fatal(err)
	}
	click := Signal{ContentRef: ref, Subject: sub, Type: "click", EventID: "click", OccurredAt: when(5)}
	if err := st.RecordSignals(ctx, "doujins", []Signal{click}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	history, err := st.History(ctx, "doujins", sub, HistoryOptions{Status: HistorySeen, Since: when(1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ContentID != ref.ContentID || !history[0].LastReadAt.Equal(when(4)) || history[0].Resume != "p:4" || history[0].MaxProgress != 4 {
		t.Fatalf("repaired reading history must retain actual read recency and resume: %+v", history)
	}
}
