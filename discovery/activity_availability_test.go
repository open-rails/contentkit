package discovery

import (
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
)

func TestActivityAvailabilityUsesImmutableIntervals(t *testing.T) {
	f := newFixture(t, nil, "")
	start := f.now.Add(-48 * time.Hour)
	f.now = start
	f.seed(item{n: 1, titles: map[string]string{"en": "Availability"}, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}})
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	switchAt := start.Add(12 * time.Hour)
	f.clock = switchAt
	public := access.Rule{Level: access.Public}
	if err := f.cat.Update(f.ctx, staff, []catalog.Update{{ID: f.cid(1), ContentPatch: catalog.ContentPatch{Access: &public}}}); err != nil {
		t.Fatal(err)
	}
	removedAt := start.Add(36 * time.Hour)
	f.clock = removedAt
	if err := f.cat.DeleteForModeration(f.ctx, staff, []string{f.cid(1)}); err != nil {
		t.Fatal(err)
	}
	f.svc.o.Now = func() time.Time { return start.Add(48 * time.Hour) }
	got, err := f.svc.Availability(f.ctx, "gallery", signal.AllTime(), signal.ActivityScope{Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Known || !got.From.Equal(start) || len(got.Intervals) != 2 {
		t.Fatalf("availability = %+v", got)
	}
	byAudience := map[string]signal.AccessInterval{}
	for _, interval := range got.Intervals {
		byAudience[interval.Audience] = interval
	}
	paid, free := byAudience["tier:premium"], byAudience["public"]
	if !paid.From.Equal(start) || !paid.Until.Equal(switchAt) || !free.From.Equal(switchAt) || !free.Until.Equal(removedAt) {
		t.Fatalf("past access changed with current deleted/public record: paid=%+v free=%+v", paid, free)
	}
}

func TestActivityAvailabilitySplitsReleasesAndUnionsEditions(t *testing.T) {
	f := newFixture(t, nil, "")
	start := f.now.Add(-24 * time.Hour)
	f.now = start
	freeAt := start.Add(6 * time.Hour)
	f.seed(item{n: 1, titles: map[string]string{"en": "Scheduled access"}, rule: `{"level":"tier","tiers":["premium"],"releases":[{"at":"` + freeAt.Format(time.RFC3339) + `","level":"public"}]}`,
		versions: []ver{{langs: []string{"en"}}, {langs: []string{"en"}}}})
	f.svc.o.Now = func() time.Time { return start.Add(24 * time.Hour) }
	got, err := f.svc.Availability(f.ctx, "gallery", signal.AllTime(), signal.ActivityScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Intervals) != 2 {
		t.Fatalf("duplicate editions multiplied intervals: %+v", got)
	}
	for _, interval := range got.Intervals {
		switch interval.Audience {
		case "tier:premium":
			if !interval.From.Equal(start) || !interval.Until.Equal(freeAt) {
				t.Fatalf("paid %+v", interval)
			}
		case "public":
			if !interval.From.Equal(freeAt) || !interval.Until.Equal(start.Add(24*time.Hour)) {
				t.Fatalf("free %+v", interval)
			}
		default:
			t.Fatalf("unexpected %+v", interval)
		}
	}
}

func TestUnionAccessIntervalsDoesNotSplitOnIrrelevantEdits(t *testing.T) {
	at := time.Now().UTC()
	got := unionAccessIntervals([]signal.AccessInterval{
		{ContentID: "work", Audience: "public", From: at, Until: at.Add(time.Hour)},
		{ContentID: "work", Audience: "public", From: at.Add(time.Hour), Until: at.Add(2 * time.Hour)},
		{ContentID: "work", Audience: "public", From: at.Add(30 * time.Minute), Until: at.Add(90 * time.Minute)},
	})
	if len(got) != 1 || got[0].ID == "" || !got[0].From.Equal(at) || !got[0].Until.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("merged = %+v", got)
	}
}

func TestWorkAudienceUnionsConcurrentEditions(t *testing.T) {
	at := time.Now().UTC()
	paid := signal.AccessInterval{ContentID: "work", Audience: "tier:premium", AudienceTiers: []string{"premium"}, From: at, Until: at.Add(3 * time.Hour)}
	free := signal.AccessInterval{ContentID: "work", Audience: "public", From: at.Add(time.Hour), Until: at.Add(2 * time.Hour)}
	got := unionAccessIntervals([]signal.AccessInterval{paid, free})
	if len(got) != 3 || got[0].Audience != "tier:premium" || got[1].Audience != "public" || got[2].Audience != "tier:premium" {
		t.Fatalf("public edition must make the work public: %+v", got)
	}
	other := paid
	other.Audience, other.AudienceTiers = "tier:gold", []string{"gold"}
	got = unionAccessIntervals([]signal.AccessInterval{paid, other})
	if len(got) != 1 || got[0].Audience != "tier:gold,premium" {
		t.Fatalf("tier OR: %+v", got)
	}
	unknown := paid
	unknown.Audience, unknown.AudienceTiers = "unknown", nil
	if got = unionAccessIntervals([]signal.AccessInterval{paid, unknown}); len(got) != 0 {
		t.Fatalf("unknown edition falsely claimed complete audience: %+v", got)
	}
	got = unionAccessIntervals([]signal.AccessInterval{paid, free, unknown})
	if len(got) != 1 || got[0].Audience != "public" || !got[0].From.Equal(free.From) || !got[0].Until.Equal(free.Until) {
		t.Fatalf("independent public edition should remain measurable: %+v", got)
	}
}

// Retiring a listing policy must not invent earlier public availability. The
// migration appends a policy-free boundary snapshot and leaves history intact.
func TestActivityAvailabilityPreservesLegacyListingUntilAdoption(t *testing.T) {
	for _, legacy := range []string{"listed", "members", "unlisted"} {
		t.Run(legacy, func(t *testing.T) {
			f := newFixture(t, nil, "")
			start := f.now.Add(-48 * time.Hour)
			f.now = start
			f.seed(item{n: 1, titles: map[string]string{"en": "Existing publication"}, versions: []ver{{langs: []string{"en"}}}})
			f.exec(`UPDATE {s}.content_access_history SET snapshot=jsonb_set(snapshot,'{listing}',to_jsonb($2::text)) WHERE tenant_id=$1`, tenant, legacy)
			adopted := start.Add(24 * time.Hour)
			f.exec(`INSERT INTO {s}.content_access_history (tenant_id,content_id,version_id,occurred_at,snapshot)
SELECT tenant_id,content_id,version_id,$2,snapshot-'listing' FROM (
 SELECT DISTINCT ON (version_id) tenant_id,content_id,version_id,snapshot
 FROM {s}.content_access_history WHERE tenant_id=$1
 ORDER BY version_id,occurred_at DESC,id DESC) latest`, tenant, adopted)
			until := start.Add(48 * time.Hour)
			f.svc.o.Now = func() time.Time { return until }
			got, err := f.svc.Availability(f.ctx, "gallery", signal.AllTime(), signal.ActivityScope{Language: "en"})
			if err != nil {
				t.Fatal(err)
			}
			wantFrom := adopted
			if legacy == "listed" {
				wantFrom = start
			}
			if len(got.Intervals) != 1 || !got.Intervals[0].From.Equal(wantFrom) || !got.Intervals[0].Until.Equal(until) {
				t.Fatalf("legacy %s: intervals=%+v, want %s..%s", legacy, got.Intervals, wantFrom, until)
			}
		})
	}
}
