package signal

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// #879: legacy per-parent fan-out rows are inventoried, then removed for one
// tenant only, from signals, projections and pairs.
func TestIntegrationInventoryAndPurgeContentKinds(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	// Keep superseded revisions physically present so raw rows are deterministic.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+".signals"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "SYSTEM START MERGES "+testDB+".signals") })
	at := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	videoID, tagID, creatorID := cid(1), cid(7), cid(9)
	mk := func(tenant, kind, id, subject, typ, eventID string, rev uint64) Signal {
		return Signal{ContentRef: contentref.New(tenant, kind, id), Subject: Subject{UserID: subject},
			Type: typ, EventID: eventID, Revision: rev, OccurredAt: at, Progress: 1, ProgressMax: 2}
	}
	for _, tenant := range []string{"hentai0", "doujins"} {
		// A checkpoint revision arrives in a later request, so it is a separate raw row.
		for _, batch := range [][]Signal{{
			mk(tenant, "video", videoID, "u1", TypeView, "s1", 1), mk(tenant, "tag", tagID, "u1", TypeView, "s1:tag:7", 1),
			mk(tenant, "creator", creatorID, "u1", TypeView, "s1:creator:9", 1),
			mk(tenant, "video", videoID, "u2", TypeView, "s2", 0), mk(tenant, "tag", tagID, "u2", TypeView, "s2:tag:7", 0),
			mk(tenant, "video", videoID, "u2", "like", "l2", 0),
		}, {
			mk(tenant, "video", videoID, "u1", TypeView, "s1", 2), mk(tenant, "tag", tagID, "u1", TypeView, "s1:tag:7", 2),
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
	type key struct{ k, s string }
	got := map[key][4]uint64{}
	for _, r := range inv {
		got[key{r.ContentKind, r.SignalType}] = [4]uint64{r.Events, r.RawRows, r.Subjects, r.ContentItems}
		if !r.FirstAt.Equal(at) || !r.LastAt.Equal(at) {
			t.Fatalf("inventory times: %+v", r)
		}
	}
	want := map[key][4]uint64{
		{"video", TypeView}: {2, 3, 2, 1}, {"video", "like"}: {1, 1, 1, 1},
		{"tag", TypeView}: {2, 3, 2, 1}, {"creator", TypeView}: {1, 1, 1, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory %v want %v", got, want)
	}

	// Mutations run through the merge machinery.
	if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".signals"); err != nil {
		t.Fatal(err)
	}
	if err := st.PurgeContentKinds(ctx, "hentai0", []string{"tag", "creator", "season"}); err != nil {
		t.Fatal(err)
	}
	inv, err = st.Inventory(ctx, "hentai0")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range inv {
		if r.ContentKind != "video" {
			t.Fatalf("purged kind remains: %+v", r)
		}
	}
	if len(inv) != 2 {
		t.Fatalf("video rows must remain: %+v", inv)
	}
	for _, tenant := range []string{"hentai0", "doujins"} {
		co, err := st.CoEngaged(ctx, tenant, contentref.New(tenant, "video", videoID), CoEngagedOptions{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		hist, err := st.History(ctx, tenant, Subject{UserID: "u1"}, HistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		tag := contentref.New(tenant, "tag", tagID)
		m, err := st.Metrics(ctx, tenant, []ContentRef{tag}, AllTime())
		if err != nil {
			t.Fatal(err)
		}
		if tenant == "hentai0" {
			if len(co) != 0 || len(hist) != 1 || hist[0].ContentKind != "video" || len(m) != 0 {
				t.Fatalf("hentai0 purge incomplete: pairs=%v history=%v metrics=%v", co, hist, m)
			}
		} else if len(co) != 2 || len(hist) != 3 || m[tag.Key()].Views != 2 {
			t.Fatalf("doujins must be untouched: pairs=%v history=%v metrics=%v", co, hist, m)
		}
	}
	if err := st.PurgeContentKinds(ctx, "hentai0", []string{"tag"}); err != nil {
		t.Fatalf("purge is idempotent: %v", err)
	}
	// Forget clears one work (and its versions) for one subject only.
	edition := contentref.NewVersion("doujins", "video", videoID, "v1")
	if err := st.RecordSignals(ctx, "doujins", []Signal{{ContentRef: edition, Subject: Subject{UserID: "u1"}, Type: TypeView, EventID: "ed", OccurredAt: at, Progress: 1, ProgressMax: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Forget(ctx, "doujins", Subject{UserID: "u1"}, "video", videoID); err != nil {
		t.Fatal(err)
	}
	states, err := st.States(ctx, "doujins", Subject{UserID: "u1"}, []ContentRef{edition, edition.Content()})
	if err != nil || len(states) != 0 {
		t.Fatalf("forget must clear the work and its versions: %+v %v", states, err)
	}
	if hist, err := st.History(ctx, "doujins", Subject{UserID: "u2"}, HistoryOptions{ContentKind: "video"}); err != nil || len(hist) != 1 {
		t.Fatalf("another subject's video history must survive: %+v %v", hist, err)
	}
}
