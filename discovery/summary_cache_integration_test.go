package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

// A list's counts are reused until the catalog records a change or the cache
// entry expires, and never across viewers.
func TestListingSummaryIsReusedUntilTheCatalogChanges(t *testing.T) {
	f := newFixture(t, nil, "")
	f.svc.o.Now = func() time.Time { return f.clock }
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

type summaryBilling struct {
	*releaseBilling
	unavailable bool
}

func (b *summaryBilling) Held(ctx context.Context, user string, q access.Query) (access.Answer, error) {
	for _, key := range q.Keys {
		if b.unavailable && strings.HasPrefix(key, tenant+":content:") {
			return access.Answer{}, access.ErrUnavailable
		}
	}
	return b.releaseBilling.Held(ctx, user, q)
}

func TestListingSummaryRecoversAfterBillingOutage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     access.Mode
		filtered bool
	}{
		{name: "hide", mode: access.Hide},
		{name: "filtered hide", mode: access.Hide, filtered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			billing := &summaryBilling{releaseBilling: &releaseBilling{held: map[string]bool{}}}
			f := newFixtureBilling(t, nil, "", siteConfig(), billing)
			owned := item{n: 2, titles: map[string]string{"en": "Owned"}, age: time.Hour,
				rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}}
			if tc.filtered {
				f.group("tag", "big-breasts")
				owned.groups = []string{"big-breasts"}
			}
			f.seed(item{n: 1, titles: map[string]string{"en": "Free"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}}, owned)
			billing.held[tenant+":content:"+f.cid(2)] = true
			o := ListOptions{Reader: Reader{Actor: access.Actor{ID: "reader"}, Host: "main.test", Language: "en", Mode: tc.mode},
				Filter: Filter{Kind: "gallery"}, Summary: true}

			billing.unavailable = true
			outage, err := f.svc.List(f.ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if outage.Summary == nil || len(outage.Contents) != 1 || outage.Summary.Total != 1 || outage.Summary.FilteredCount != 0 {
				t.Fatalf("outage: cards=%d summary=%+v, want only the free item", len(outage.Contents), outage.Summary)
			}
			if tc.filtered && (len(outage.Summary.FilteredEntities) != 1 || outage.Summary.FilteredEntities[0].Count != 0) {
				t.Fatalf("outage: entity counts=%+v, want zero", outage.Summary.FilteredEntities)
			}

			// The prefix remains truncated, and neither the catalog nor the
			// clock changes. Only exact entitlement reads recover.
			billing.unavailable = false
			recovered, err := f.svc.List(f.ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			total, filtered := 2, 0
			if tc.filtered {
				total, filtered = 1, 1
			}
			if recovered.Summary == nil || len(recovered.Contents) != total || recovered.Summary.Total != total || recovered.Summary.FilteredCount != filtered || recovered.Summary.LockedCount != 0 {
				t.Fatalf("recovery: cards=%d summary=%+v, want total=%d filtered=%d locked=0", len(recovered.Contents), recovered.Summary, total, filtered)
			}
			if tc.filtered && (len(recovered.Summary.FilteredEntities) != 1 || recovered.Summary.FilteredEntities[0].Count != 1) {
				t.Fatalf("recovery: entity counts=%+v, want one", recovered.Summary.FilteredEntities)
			}
		})
	}
}
