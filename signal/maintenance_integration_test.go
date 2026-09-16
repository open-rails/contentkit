package signal

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// #879: legacy per-parent fan-out rows are inventoried, then removed for one
// tenant only, from events, projections and pairs.
func TestIntegrationInventoryAndPurgeEntityTypes(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	// Keep superseded revisions physically present so raw rows are deterministic.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+".events"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "SYSTEM START MERGES "+testDB+".events") })
	at := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	mk := func(entityType, id, subject, typ, eventID string, rev uint64) Signal {
		return Signal{EntityRef: EntityRef{EntityType: entityType, EntityID: id}, Subject: Subject{UserID: subject},
			Type: typ, EventID: eventID, Revision: rev, OccurredAt: at, Progress: 1, ProgressMax: 2}
	}
	for _, tenant := range []string{"hentai0", "doujins"} {
		// A checkpoint revision arrives in a later request, so it is a separate raw row.
		for _, batch := range [][]Signal{{
			mk("video", "1", "u1", TypeView, "s1", 1), mk("tag", "7", "u1", TypeView, "s1:tag:7", 1),
			mk("creator", "9", "u1", TypeView, "s1:creator:9", 1),
			mk("video", "1", "u2", TypeView, "s2", 0), mk("tag", "7", "u2", TypeView, "s2:tag:7", 0),
			mk("video", "1", "u2", "like", "l2", 0),
		}, {
			mk("video", "1", "u1", TypeView, "s1", 2), mk("tag", "7", "u1", TypeView, "s1:tag:7", 2),
		}} {
			if err := st.RecordSignals(ctx, tenant, batch); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := st.Inventory(ctx, "hentai0")
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ e, s string }
	got := map[key][3]uint64{}
	for _, r := range inv {
		got[key{r.EntityType, r.SignalType}] = [3]uint64{r.Events, r.RawRows, r.Subjects}
		if !r.FirstAt.Equal(at) || !r.LastAt.Equal(at) {
			t.Fatalf("inventory times: %+v", r)
		}
	}
	want := map[key][3]uint64{
		{"video", TypeView}: {2, 3, 2}, {"video", "like"}: {1, 1, 1},
		{"tag", TypeView}: {2, 3, 2}, {"creator", TypeView}: {1, 1, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory %v want %v", got, want)
	}

	// Mutations run through the merge machinery.
	if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".events"); err != nil {
		t.Fatal(err)
	}
	if err := st.PurgeEntityTypes(ctx, "hentai0", []string{"tag", "creator", "season"}); err != nil {
		t.Fatal(err)
	}
	inv, err = st.Inventory(ctx, "hentai0")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range inv {
		if r.EntityType != "video" {
			t.Fatalf("purged type remains: %+v", r)
		}
	}
	if len(inv) != 2 {
		t.Fatalf("video rows must remain: %+v", inv)
	}
	for _, tenant := range []string{"hentai0", "doujins"} {
		co, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "video", EntityID: "1"}, CoEngagedOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		hist, err := st.History(ctx, tenant, Subject{UserID: "u1"}, HistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m, err := st.Metrics(ctx, tenant, "tag", []string{"7"}, AllTime())
		if err != nil {
			t.Fatal(err)
		}
		if tenant == "hentai0" {
			if len(co) != 0 || len(hist) != 1 || hist[0].EntityType != "video" || len(m) != 0 {
				t.Fatalf("hentai0 purge incomplete: pairs=%v history=%v metrics=%v", co, hist, m)
			}
		} else if len(co) != 2 || len(hist) != 3 || m["7"].Views != 2 {
			t.Fatalf("doujins must be untouched: pairs=%v history=%v metrics=%v", co, hist, m)
		}
	}
	if err := st.PurgeEntityTypes(ctx, "hentai0", []string{"tag"}); err != nil {
		t.Fatalf("purge is idempotent: %v", err)
	}
}
