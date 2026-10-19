package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

func TestHistoricalActivityRanksWindowBeforeVisiblePagination(t *testing.T) {
	env := signaltest.FromEnv(t)
	const database = "contentkit_historical_activity_test"
	f := newFixture(t, env.Fresh(t, database), database)
	f.group("tag", "romance", "big-breasts", "yaoi")
	f.seed(item{n: 1, titles: map[string]string{"en": "Alpha"}, age: 100 * 24 * time.Hour, attrs: map[string]any{"format": "manga"}, versions: []ver{{langs: []string{"en"}, pages: 10}}, groups: []string{"romance"}},
		item{n: 2, titles: map[string]string{"en": "Bravo"}, age: 100 * 24 * time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}, pages: 10}}, groups: []string{"big-breasts"}},
		item{n: 3, titles: map[string]string{"ja": "チャーリー"}, age: 100 * 24 * time.Hour, restrictions: []string{"restricted"}, versions: []ver{{langs: []string{"ja"}, pages: 10}}, groups: []string{"yaoi"}},
		item{n: 4, titles: map[string]string{"en": "Future edition"}, age: 100 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}, future: true}}},
		item{n: 5, titles: map[string]string{"en": "Unlisted"}, age: 100 * 24 * time.Hour, listing: "unlisted", versions: []ver{{langs: []string{"en"}, pages: 10}}},
		item{n: 6, titles: map[string]string{"en": "Deleted"}, age: 100 * 24 * time.Hour, deleted: true, versions: []ver{{langs: []string{"en"}, pages: 10}}},
		item{n: 7, kind: "video", titles: map[string]string{"en": "Other content kind"}, age: 100 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, titles: map[string]string{"en": "Old publication, recent activity"}, age: 800 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}, pages: 10}}},
		item{n: 9, titles: map[string]string{"en": "Only outside the activity range"}, age: 800 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 10, titles: map[string]string{"en": "New but unviewed"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	// This fixture's catalog recorded immutable history at those actual old
	// publication clocks. Instrumentation therefore starts at its earliest row.
	f.exec(`UPDATE {s}.content_access_history_start SET started_at=(SELECT min(occurred_at) FROM {s}.content_access_history WHERE tenant_id=$1) WHERE tenant_id=$1`, tenant)
	from := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -60)
	until := from.AddDate(0, 0, 7)
	var population []signal.Subject
	for i := range 120 {
		population = append(population, signal.Subject{UserID: fmt.Sprintf("cohort-%d", i)})
	}
	// The same independently observed cohort contains at least twenty people
	// who read none of the works. Locale/domain eligibility is explicit.
	for _, domain := range []string{"main", "clean", "gay"} {
		for _, language := range []string{"en", "ja"} {
			seedPopulation(f, "gallery", signal.ActivityScope{Domain: domain, Language: language}, from, population, []string{"premium"})
		}
	}
	var observations []engagementtest.Observation
	read := func(n, count int, at time.Time, label string) {
		kind := "gallery"
		if n == 7 {
			kind = "video"
		}
		for i := range count {
			for _, domain := range []string{"main", "clean", "gay"} {
				if domain == "gay" && n != 3 || domain == "clean" && n == 3 {
					continue
				}
				language := "en"
				if n == 3 {
					language = "ja"
				}
				access := readingAccess(at, signal.ActivityScope{Domain: domain, Language: language})
				access.EligibleTiers = []string{"premium"}
				if n == 2 {
					access.ContentAccess, access.Audience, access.AudienceTiers = "restricted", "tier:premium", []string{"premium"}
				}
				observations = append(observations, engagementtest.Observation{ContentRef: contentref.NewVersion(tenant, kind, f.cid(n), f.vid(n, 0)),
					Subject: population[i], Type: engagementtest.TypeReading,
					EventID: fmt.Sprintf("%s-%s-%d", label, domain, i), OccurredAt: at, Progress: 10, ProgressMax: 10, DurationS: 100, Access: access})
			}
		}
	}
	for n, count := range map[int]int{1: 3, 2: 60, 3: 20, 4: 50, 5: 45, 6: 40, 7: 100, 8: 8} {
		read(n, count, from, fmt.Sprintf("inside-%d", n))
	}
	read(9, 80, from.Add(-time.Second), "before")
	read(9, 80, until, "exclusive-until")
	if err := engagementtest.Record(f.ctx, f.svc.o.Hub.Signals(), tenant, observations); err != nil {
		t.Fatal(err)
	}
	path := func(extra ...string) string {
		return q("/contents", append([]string{"kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly)}, extra...)...)
	}
	for _, tc := range []struct {
		host  string
		extra []string
		want  []string
	}{
		{"main.test", nil, []string{f.cid(3), f.cid(8), f.cid(1)}},
		{"main.test", []string{"language", "en"}, []string{f.cid(8), f.cid(1)}},
		{"clean.test", nil, []string{f.cid(8), f.cid(1)}},
		{"gay.test", nil, []string{f.cid(3)}},
		{"main.test", []string{"groups", f.ids["romance"]}, []string{f.cid(1)}},
		{"main.test", []string{"attribute", "format:manga"}, []string{f.cid(1)}},
		{"main.test", []string{"published_within", "7d"}, []string{}}, // no pre-publication activity can be real
	} {
		got, _ := f.list(tc.host, "", path(tc.extra...))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %v got %v, want %v", tc.host, tc.extra, got, tc.want)
		}
	}
	// Hidden higher scores must not consume visible page positions.
	got, first := f.list("clean.test", "", path("limit", "1"))
	if !reflect.DeepEqual(got, []string{f.cid(8)}) || first.NextCursor == "" {
		t.Fatalf("first visible page: %v %+v", got, first)
	}
	if r := first.Ranking; r == nil || r.Policy != "v2" || r.Mode != "measured_then_standing" || r.CoveredFrom != from.Format(time.RFC3339) {
		t.Fatalf("measured ranking provenance: %+v", r)
	}
	got, last := f.list("clean.test", "", path("limit", "1", "cursor", first.NextCursor))
	if !reflect.DeepEqual(got, []string{f.cid(1)}) || last.NextCursor != "" {
		t.Fatalf("last visible page: %v %+v", got, last)
	}
	for _, changed := range []string{
		q("/contents", "kind", "gallery", "sort", "popular", "activity_from", from.AddDate(0, 0, 1).Format(time.DateOnly), "activity_until", until.Format(time.DateOnly), "cursor", first.NextCursor),
		q("/contents", "kind", "gallery", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.AddDate(0, 0, 1).Format(time.DateOnly), "cursor", first.NextCursor),
		q("/contents", "kind", "video", "sort", "popular", "activity_from", from.Format(time.DateOnly), "activity_until", until.Format(time.DateOnly), "cursor", first.NextCursor),
		q("/contents", "kind", "gallery", "sort", "popular:7d", "cursor", first.NextCursor),
	} {
		if status, code := f.call(http.MethodGet, "main.test", "", changed, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("changed cursor scope: %d %s", status, code)
		}
	}
	// One current vote, unrelated to measured readership, distinguishes the
	// standing order from any measured result.
	f.react(1, "standing-fan", 1, from)
	// Before population lineage there is no measured ranking to fall back on.
	if got, page := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular", "activity_from", "1990-01-01", "activity_until", "1991-01-01")); len(got) != 0 ||
		page.Ranking == nil || page.Ranking.Mode != "standing" {
		t.Fatalf("uncovered historical window: %v %+v", got, page.Ranking)
	}
	// A warm measured window cannot be served through a signal-plane outage.
	store := f.svc.o.Hub.Signals()
	f.svc.o.Hub.SetSignals(nil)
	got, down := f.list("main.test", "", path())
	if !reflect.DeepEqual(got, []string{f.cid(1)}) || down.Ranking == nil || *down.Ranking != (popularity.RankingInfo{Policy: "standing_wilson", Mode: "standing",
		RequestedFrom: from.Format(time.RFC3339), RequestedUntil: until.Format(time.RFC3339)}) || down.Contents[0].Ranking != nil {
		t.Fatalf("down signal plane: %v %+v", got, down.Ranking)
	}
	f.svc.o.Hub.SetSignals(store)
}

func TestHistoricalActivityHTTPValidationWithoutSignalPlane(t *testing.T) {
	f := seeded(t)
	f.exec(`UPDATE {s}.content_facets SET popularity='{"score":{"all":99},"rating":{"all":1}}' WHERE content_id=$1`, f.cid(1))
	valid := q("/contents", "kind", "gallery", "sort", "popular", "activity_from", "2020-01-01", "activity_until", "2021-01-01")
	if got, page := f.list("main.test", "", valid); len(got) != 0 || page.Ranking == nil || page.Ranking.Policy != "standing_wilson" {
		t.Fatalf("no signal plane must be labeled standing, never preset scores: %v %+v", got, page.Ranking)
	}
	for _, path := range []string{
		valid + "&activity_from=2020-02-01",
		q("/contents", "kind", "gallery", "sort", "popular", "activity_from", "", "activity_until", ""),
		q("/contents", "kind", "gallery", "sort", "popular:7d", "activity_from", "2020-01-01", "activity_until", "2021-01-01"),
		q("/contents", "kind", "gallery", "sort", "newest", "activity_from", "2020-01-01", "activity_until", "2021-01-01"),
	} {
		if status, code := f.call(http.MethodGet, "main.test", "", path, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("invalid activity query %s: %d %s", path, status, code)
		}
	}
}

func TestNamedPopularExcludesWorksWithoutStanding(t *testing.T) {
	f := seeded(t)
	f.seed(item{n: 8, titles: map[string]string{"en": "Unrated"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "Disliked"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	at := f.now.Add(-time.Hour)
	f.react(3, "a", 1, at)
	f.react(3, "b", 1, at)
	f.react(1, "a", 1, at)
	f.react(9, "a", -1, at)
	f.react(1, "old", 1, f.now.AddDate(0, 0, -40))
	f.react(1, "older", 1, f.now.AddDate(0, 0, -400))
	for _, period := range []string{"7d", "all"} {
		got, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular:"+period))
		want := []string{f.cid(3), f.cid(1)}
		if period == "all" {
			want = []string{f.cid(1), f.cid(3)}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("popular:%s got %v, want %v", period, got, want)
		}
	}
}
