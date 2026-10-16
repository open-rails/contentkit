package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

func TestHistoricalActivityRanksWindowBeforeVisiblePagination(t *testing.T) {
	env := signaltest.FromEnv(t)
	const database = "contentkit_historical_activity_test"
	f := seededOn(t, env.Fresh(t, database), database)
	f.seed(item{n: 8, titles: map[string]string{"en": "Old publication, recent activity"}, age: 800 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "Only outside the activity range"}, age: 800 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 10, titles: map[string]string{"en": "New but unviewed"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	from := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -60)
	until := from.AddDate(0, 0, 7)
	var events []signal.Signal
	view := func(n, count int, at time.Time, label string) {
		kind := "gallery"
		if n == 7 {
			kind = "video"
		}
		for i := range count {
			events = append(events, signal.Signal{ContentRef: contentref.New(tenant, kind, f.cid(n)),
				Subject: signal.Subject{UserID: fmt.Sprintf("%s-%d", label, i)}, Type: signal.TypeView,
				EventID: fmt.Sprintf("%s-%d", label, i), OccurredAt: at, Progress: 10, ProgressMax: 10, DurationS: 100})
		}
	}
	for n, count := range map[int]int{1: 3, 2: 60, 3: 20, 4: 50, 5: 45, 6: 40, 7: 100, 8: 8} {
		view(n, count, from, fmt.Sprintf("inside-%d", n))
	}
	view(9, 80, from.Add(-time.Second), "before")
	view(9, 80, until, "exclusive-until")
	if err := f.svc.o.Hub.Signals().RecordSignals(f.ctx, tenant, events); err != nil {
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
		{"main.test", []string{"published_within", "7d"}, []string{f.cid(3), f.cid(1)}},
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
	if got, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular", "activity_from", "1990-01-01", "activity_until", "1991-01-01")); len(got) != 0 {
		t.Fatalf("empty historical window fell back: %v", got)
	}
	// Even a warm cached window cannot bypass a known signal-plane outage.
	store := f.svc.o.Hub.Signals()
	f.svc.o.Hub.SetSignals(nil)
	if status, code := f.call(http.MethodGet, "main.test", "", path(), nil, nil); status != http.StatusServiceUnavailable || code != "unavailable" {
		t.Fatalf("down signal plane: %d %s", status, code)
	}
	f.svc.o.Hub.SetSignals(store)
}

func TestHistoricalActivityHTTPValidationAndNoFallback(t *testing.T) {
	f := seeded(t)
	valid := q("/contents", "kind", "gallery", "sort", "popular", "activity_from", "2020-01-01", "activity_until", "2021-01-01")
	if status, code := f.call(http.MethodGet, "main.test", "", valid, nil, nil); status != http.StatusNotImplemented || code != "not_configured" {
		t.Fatalf("no signal plane must not use preset scores: %d %s", status, code)
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

func TestNamedPopularExcludesZeroActivityScores(t *testing.T) {
	f := seeded(t)
	f.seed(item{n: 8, titles: map[string]string{"en": "Unviewed"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	for _, period := range []string{"7d", "all"} {
		got, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular:"+period))
		want := []string{f.cid(3), f.cid(1)}
		if period == "all" {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("popular:%s includes unviewed work: got %v, want %v", period, got, want)
		}
	}
}
