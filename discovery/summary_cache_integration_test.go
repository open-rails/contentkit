package discovery

import (
	"testing"
	"time"
)

// A list's counts are reused until the catalog records a change or the cache
// entry expires, and never across viewers.
func TestListingSummaryIsReusedUntilTheCatalogChanges(t *testing.T) {
	f := newFixture(t, nil, "")
	f.seed(item{n: 1, titles: map[string]string{"en": "Free"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Premium"}, age: 2 * time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}})
	const host = "main.test"
	path := q("/contents", "kind", "gallery", "limit", "1", "summary", "true")
	summary := func(user string) ListingSummary {
		t.Helper()
		_, page := f.list(host, user, path)
		if page.Summary == nil {
			t.Fatalf("%q: no summary", user)
		}
		return *page.Summary
	}
	if s := summary(""); s.Total != 2 || s.LockedCount != 1 {
		t.Fatalf("first summary = %+v, want total 2, locked 1", s)
	}
	if s := summary("fan"); s.Total != 2 || s.LockedCount != 0 {
		t.Fatalf("a premium reader got %+v, want total 2, locked 0 (counts are per viewer)", s)
	}

	// A change that records no catalog event shows once the entry expires.
	f.exec(`UPDATE `+f.table("content_facets")+` SET deleted = true WHERE content_id = $1`, f.cid(1))
	if s := summary(""); s.Total != 2 {
		t.Fatalf("summary before expiry = %+v, want the cached total 2", s)
	}
	later := f.clock.Add(summaryTTL + time.Second)
	f.svc.o.Now = func() time.Time { return later }
	if s := summary(""); s.Total != 1 {
		t.Fatalf("summary after expiry = %+v, want total 1", s)
	}
	f.svc.o.Now = func() time.Time { return f.clock }

	// A catalog write starts fresh counts at once.
	f.seed(item{n: 3, titles: map[string]string{"en": "New"}, age: time.Minute, versions: []ver{{langs: []string{"en"}}}})
	if s := summary(""); s.Total != 2 {
		t.Fatalf("summary after a catalog write = %+v, want total 2", s)
	}
}
