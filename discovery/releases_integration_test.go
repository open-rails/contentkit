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
		from, today, _, err := releaseWindow(at, tc.days, tc.zone)
		if err != nil || from.Format(time.DateOnly) != tc.from || today.Format(time.DateOnly) != tc.through || today.Sub(from).Hours() != tc.hours {
			t.Fatalf("%+v: %v %v %v", tc, from, today, err)
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
		item{n: 6, titles: map[string]string{"en": "Unlisted"}, age: time.Hour, listing: "unlisted", versions: []ver{{langs: []string{"en"}}}},
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
				t.Fatal("publication feed exposed future, unlisted or deleted content to staff")
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
		item{n: 1, titles: map[string]string{"en": "Recent"}, age: time.Hour, popularity: `{"score":{"7d":4,"90d":4,"365d":4,"all":4}}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Quarter"}, age: 8 * 24 * time.Hour, popularity: `{"score":{"7d":100,"90d":3,"365d":3,"all":3}}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Year"}, age: 100 * 24 * time.Hour, popularity: `{"score":{"7d":100,"90d":100,"365d":2,"all":2}}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Old"}, age: 400 * 24 * time.Hour, popularity: `{"score":{"7d":100,"90d":100,"365d":100,"all":1}}`, versions: []ver{{langs: []string{"en"}}}},
	)
	for _, tc := range []struct {
		period string
		count  int
	}{{"7d", 1}, {"90d", 2}, {"365d", 3}, {"all", 4}} {
		got, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", "popular:"+tc.period, "published_within", tc.period))
		if len(got) != tc.count || got[0] != f.cid(1) {
			t.Fatalf("%s: %v", tc.period, got)
		}
	}
	// The first page pins the age window, even when a later page crosses UTC
	// midnight. New publications after that point cannot enter the sequence.
	f.seed(item{n: 5, titles: map[string]string{"en": "Window boundary"}, age: 6*24*time.Hour + 11*time.Hour, popularity: `{"score":{"7d":2}}`, versions: []ver{{langs: []string{"en"}}}})
	o := ListOptions{Reader: Reader{Actor: access.Actor{Anonymous: true}, Host: "main.test"}, Filter: Filter{Kind: "gallery", PublishedWithin: "7d"}, Sort: "popular:7d", Limit: 1}
	first, err := f.svc.List(f.ctx, o)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first %+v %v", first, err)
	}
	f.now = f.now.Add(24 * time.Hour)
	f.clock = f.now
	f.seed(item{n: 6, titles: map[string]string{"en": "New publication"}, age: time.Hour, popularity: `{"score":{"7d":1}}`, versions: []ver{{langs: []string{"en"}}}})
	o.Cursor = first.NextCursor
	next, err := f.svc.List(f.ctx, o)
	if err != nil || len(next.Contents) != 1 || next.Contents[0].ID != f.cid(5) || next.NextCursor != "" {
		t.Fatalf("pinned next %+v %v", next, err)
	}
}
