package discovery

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

func TestPurchasedLibraryUsesBillingEntitlementsAndDomainPolicy(t *testing.T) {
	b := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: true}
	f := newFixtureBilling(t, nil, "", siteConfig(), b)
	f.entity("tag", "restricted")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Whole work"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 2, titles: map[string]string{"en": "Hard hidden"}, age: time.Hour, rule: `{"level":"ppv"}`, entities: []string{"restricted"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "One version"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
	)
	key, _ := access.ContentKey(tenant, f.cid(1))
	b.held[key.String()] = true
	hidden, _ := access.ContentKey(tenant, f.cid(2))
	b.held[hidden.String()] = true
	version, _ := access.VersionKey(tenant, f.cid(3), f.vid(3, 1))
	b.held[version.String()] = true
	// An unrelated tenant's entitlement never selects local content.
	foreign, _ := access.ContentKey("foreign", f.cid(3))
	b.held[foreign.String()] = true
	var page LibraryPage
	if status, _ := f.call(http.MethodGet, "clean.test", "reader", "/me/library?language=en", nil, &page); status != http.StatusOK {
		t.Fatalf("library status=%d", status)
	}
	if len(page.Items) != 2 || !page.Complete {
		t.Fatalf("library=%+v", page)
	}
	got := map[string]LibraryItem{}
	for _, it := range page.Items {
		got[it.ContentID] = it
	}
	if !got[f.cid(1)].Whole || len(got[f.cid(1)].VersionIDs) != 0 {
		t.Fatalf("whole purchase=%+v", got[f.cid(1)])
	}
	if got[f.cid(3)].Whole || len(got[f.cid(3)].VersionIDs) != 1 || got[f.cid(3)].VersionIDs[0] != f.vid(3, 1) {
		t.Fatalf("version purchase=%+v", got[f.cid(3)])
	}
	if _, exists := got[f.cid(2)]; exists {
		t.Fatal("hard hidden purchase leaked")
	}
	if status, _ := f.call(http.MethodGet, "clean.test", "", "/me/library", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous library status=%d", status)
	}
	if status, _ := f.call(http.MethodGet, "clean.test", "reader", "/me/library?limit=0", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("invalid page size status=%d", status)
	}
	b.unavailable = true
	if status, _ := f.call(http.MethodGet, "clean.test", "reader", "/me/library", nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("billing failure status=%d", status)
	}
}
