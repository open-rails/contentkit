package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

func TestReleaseWindowCalendarDays(t *testing.T) {
	for _, tc := range []struct {
		at, zone, from, through string
		days                    int
		hours                   float64
	}{
		{"2026-10-16T02:00:00Z", "America/Denver", "2026-10-06", "2026-10-15", 10, 216},
		{"2026-11-03T12:00:00Z", "America/Denver", "2026-11-01", "2026-11-03", 3, 49},
		{"2026-03-10T12:00:00Z", "America/Denver", "2026-03-08", "2026-03-10", 3, 47},
		{"2026-10-16T02:00:00Z", "", "2026-10-07", "2026-10-16", 0, 216},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		from, today, _, err := releaseWindow(at, tc.days, tc.zone, "")
		if err != nil || from.Format(time.DateOnly) != tc.from || today.Format(time.DateOnly) != tc.through || today.Sub(from).Hours() != tc.hours {
			t.Fatalf("%+v: %v %v %v", tc, from, today, err)
		}
	}
}

func TestReleaseWindowHistoricalDates(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-16T02:00:00Z")
	for _, tc := range []struct {
		date, zone, from string
		days             int
		hours            float64
	}{
		{"2007-03-12", "America/Denver", "2007-03-10", 3, 47},
		{"2020-11-02", "America/Denver", "2020-10-31", 3, 49},
		{"2000-02-29", "UTC", "2000-02-29", 1, 0},
		{"0001-01-01", "UTC", "0001-01-01", 10, 0},
	} {
		from, through, _, err := releaseWindow(now, tc.days, tc.zone, tc.date)
		if err != nil || from.Format(time.DateOnly) != tc.from || through.Format(time.DateOnly) != tc.date || through.Sub(from).Hours() != tc.hours {
			t.Fatalf("%+v: %v %v %v", tc, from, through, err)
		}
	}
	for _, date := range []string{"2026-10-16", "2027-01-01", "2026-02-30", "1900-02-29", "2026-1-01", "2026-01", "0000-01-01", "nonsense"} {
		if _, _, _, err := releaseWindow(now, 10, "America/Denver", date); err == nil {
			t.Fatalf("accepted invalid or locally future date %q", date)
		}
	}
}

func TestDailyReleasesHistoricalWindowKeepsBoundsAndSummaries(t *testing.T) {
	f := newFixture(t, nil, "")
	f.now, _ = time.Parse(time.RFC3339, "2026-10-16T02:00:00Z")
	f.clock = f.now
	f.svc.o.Now = func() time.Time { return f.now }
	f.group("tag", "big-breasts")
	age := func(raw string) time.Duration {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatal(err)
		}
		return f.now.Sub(at)
	}
	f.seed(
		item{n: 1, titles: map[string]string{"en": "First midnight"}, age: age("2010-03-13T07:00:00Z"), versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Last instant"}, age: age("2010-03-15T05:59:59Z"), versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Next midnight"}, age: age("2010-03-15T06:00:00Z"), versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Before window"}, age: age("2010-03-13T06:59:59Z"), versions: []ver{{langs: []string{"en"}}}},
		item{n: 5, titles: map[string]string{"en": "Removed later"}, age: age("2010-03-14T12:00:00Z"), deleted: true, versions: []ver{{langs: []string{"en"}}}},
		item{n: 6, titles: map[string]string{"en": "Historical premium"}, age: age("2010-03-14T12:00:00Z"), rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 7, titles: map[string]string{"en": "Filtered in window"}, age: age("2010-03-14T12:00:00Z"), groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, titles: map[string]string{"en": "Filtered after window"}, age: age("2010-03-15T06:00:00Z"), groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	var out DailyReleases
	if status, code := f.call(http.MethodGet, "main.test", "", "/releases?kind=gallery&days=2&end_date=2010-03-14&time_zone=America%2FDenver", nil, &out); status != http.StatusOK {
		t.Fatalf("%d %s", status, code)
	}
	if out.FromDate != "2010-03-13" || out.ThroughDate != "2010-03-14" || len(out.Days) != 2 {
		t.Fatalf("unexpected historical extent %+v", out)
	}
	got := map[string]string{}
	for _, day := range out.Days {
		for _, card := range day.Contents {
			got[card.ID] = day.Date
			if card.ID == f.cid(6) && card.Access.Allowed {
				t.Fatal("historical selection granted premium access")
			}
		}
	}
	want := map[string]string{f.cid(1): "2010-03-13", f.cid(2): "2010-03-14", f.cid(6): "2010-03-14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("historical releases %v want %v", got, want)
	}
	if day := out.Days[0]; day.FilteredCount != 1 || len(day.FilteredEntities) != 1 || day.FilteredEntities[0].Count != 1 {
		t.Fatalf("historical filtered counts %+v", day)
	}
	if out.Days[1].FilteredCount != 0 {
		t.Fatalf("other day's filtered count %+v", out.Days[1])
	}
	for _, date := range []string{"2026-10-16", "2026-02-30", "2026-01", "0000-01-01"} {
		if status, _ := f.call(http.MethodGet, "main.test", "", "/releases?end_date="+date+"&time_zone=America%2FDenver", nil, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid date %s: %d", date, status)
		}
	}
}

func TestDailyReleasesWindowAndVisibility(t *testing.T) {
	f := newFixture(t, nil, "")
	f.now, _ = time.Parse(time.RFC3339, "2026-10-16T02:00:00Z")
	f.clock = f.now
	f.svc.o.Now = func() time.Time { return f.now }
	f.group("tag", "big-breasts")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Today"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Yesterday"}, age: 22 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "First day"}, age: 9 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Outside"}, age: 10 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 5, titles: map[string]string{"en": "Future"}, age: time.Hour, versions: []ver{{langs: []string{"en"}, future: true}}},
		item{n: 6, titles: map[string]string{"en": "Draft"}, age: time.Hour, draft: true, versions: []ver{{langs: []string{"en"}}}},
		item{n: 7, titles: map[string]string{"en": "Deleted"}, age: time.Hour, deleted: true, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, titles: map[string]string{"en": "Premium"}, age: time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "Filtered"}, age: time.Hour, groups: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	var out DailyReleases
	if status, code := f.call(http.MethodGet, "main.test", "", "/releases?kind=gallery&time_zone=America%2FDenver", nil, &out); status != http.StatusOK {
		t.Fatalf("%d %s", status, code)
	}
	if out.TimeZone != "America/Denver" || out.FromDate != "2026-10-06" || out.ThroughDate != "2026-10-15" || len(out.Days) != 10 {
		t.Fatalf("window %+v", out)
	}
	got := map[string]string{}
	for _, d := range out.Days {
		for _, c := range d.Contents {
			got[c.ID] = d.Date
			if c.ID == f.cid(8) && c.Access.Allowed {
				t.Fatal("premium access granted")
			}
		}
	}
	want := map[string]string{f.cid(1): "2026-10-15", f.cid(2): "2026-10-14", f.cid(3): "2026-10-06", f.cid(8): "2026-10-15"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("releases %v want %v", got, want)
	}
	if len(out.Days[2].Contents) != 0 {
		t.Fatal("empty calendar bucket omitted")
	}
	if status, _ := f.call(http.MethodGet, "main.test", "staff", "/releases?kind=gallery&time_zone=America%2FDenver", nil, &out); status != http.StatusOK {
		t.Fatal(status)
	}
	for _, d := range out.Days {
		for _, c := range d.Contents {
			if c.ID == f.cid(5) || c.ID == f.cid(6) || c.ID == f.cid(7) {
				t.Fatal("publication feed exposed future, draft or deleted content to staff")
			}
		}
	}
	if status, _ := f.call(http.MethodGet, "main.test", "", "/releases?kind=gallery&time_zone=America%2FDenver&mode=hide", nil, &out); status != http.StatusOK {
		t.Fatal(status)
	}
	for _, d := range out.Days {
		for _, c := range d.Contents {
			if c.ID == f.cid(8) {
				t.Fatal("locked card returned in hide mode")
			}
		}
	}
	for _, path := range []string{"/releases?days=32", "/releases?days=-1", "/releases?time_zone=NoSuchZone", "/releases?time_zone=Local"} {
		if status, _ := f.call(http.MethodGet, "main.test", "", path, nil, nil); status != http.StatusBadRequest {
			t.Fatalf("%s: %d", path, status)
		}
	}
}

func TestDailyReleasesDoesNotTruncateCardMetadata(t *testing.T) {
	f := newFixture(t, nil, "")
	items := make([]item, 260)
	for i := range items {
		items[i] = item{n: i + 1, titles: map[string]string{"en": fmt.Sprintf("Complete metadata %d", i)}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}}
	}
	f.seed(items...)
	out, err := f.svc.Releases(f.ctx, DailyReleasesOptions{Reader: Reader{Actor: access.Actor{Anonymous: true}, Host: "main.test"}, Filter: Filter{Kind: "gallery"}, Days: 2})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range out.Days {
		for _, c := range d.Contents {
			if c.ID == "" || c.Title == "" || c.Path == "" {
				t.Fatalf("incomplete card %+v", c)
			}
			if seen[c.ID] {
				t.Fatalf("duplicate %s", c.ID)
			}
			seen[c.ID] = true
		}
	}
	if len(seen) != len(items) {
		t.Fatalf("returned %d of %d cards", len(seen), len(items))
	}
}

func TestPopularityPublicationWindowAndCursor(t *testing.T) {
	f := newFixture(t, nil, "")
	f.now, _ = time.Parse(time.RFC3339, "2026-10-15T12:00:00Z")
	f.clock = f.now
	f.svc.o.Now = func() time.Time { return f.now }
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Recent"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Quarter"}, age: 8 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Year"}, age: 100 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Old"}, age: 400 * 24 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
	)
	// Older works have more current standing this week; publication age alone
	// keeps them out of the shorter publication windows.
	votes := func(n, people int) {
		for i := range people {
			f.react(n, fmt.Sprintf("fan-%d", i), 1, f.now.Add(-30*time.Minute))
		}
	}
	votes(1, 3)
	votes(2, 4)
	votes(3, 5)
	votes(4, 6)
	for _, tc := range []struct {
		period string
		want   []string
	}{{"7d", []string{f.cid(1)}}, {"90d", []string{f.cid(2), f.cid(1)}}, {"365d", []string{f.cid(3), f.cid(2), f.cid(1)}}, {"all", []string{f.cid(4), f.cid(3), f.cid(2), f.cid(1)}}} {
		if got, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular:"+tc.period, "published_within", tc.period)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: %v, want %v", tc.period, got, tc.want)
		}
	}
	// The first page pins the age window, even when a later page crosses UTC
	// midnight. New publications after that point cannot enter the sequence.
	f.seed(item{n: 5, titles: map[string]string{"en": "Window boundary"}, age: 6*24*time.Hour + 11*time.Hour, versions: []ver{{langs: []string{"en"}}}})
	votes(5, 2)
	o := ListOptions{Reader: Reader{Actor: access.Actor{Anonymous: true}, Host: "main.test"}, Filter: Filter{Kind: "gallery", PublishedWithin: "7d"}, Sort: "popular:7d", Limit: 1}
	first, err := f.svc.List(f.ctx, o)
	if err != nil || len(first.Contents) != 1 || first.Contents[0].ID != f.cid(1) || first.NextCursor == "" {
		t.Fatalf("first %+v %v", first, err)
	}
	f.now = f.now.Add(24 * time.Hour)
	f.clock = f.now
	f.seed(item{n: 6, titles: map[string]string{"en": "New publication"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}})
	votes(6, 1)
	o.Cursor = first.NextCursor
	next, err := f.svc.List(f.ctx, o)
	if err != nil || len(next.Contents) != 1 || next.Contents[0].ID != f.cid(5) || next.NextCursor != "" {
		t.Fatalf("pinned next %+v %v", next, err)
	}
}
