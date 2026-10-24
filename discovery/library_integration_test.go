package discovery

import (
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
)

func TestPurchasedLibraryUsesBillingEntitlementsAndDomainPolicy(t *testing.T) {
	b := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: true}
	cfg := siteConfig()
	k := cfg.Kinds["gallery"]
	k.TitlePolicy = kinds.TitleOriginalAndVersion
	cfg.Kinds["gallery"] = k
	tag := cfg.EntityKinds["tag"]
	tag.MembershipScopes = []kinds.MembershipScope{kinds.ContentScope, kinds.VersionScope}
	cfg.EntityKinds["tag"] = tag
	f := newFixtureBilling(t, nil, "", cfg, b)
	f.entity("tag", "restricted")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Whole work"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 2, titles: map[string]string{"en": "Hard hidden"}, age: time.Hour, rule: `{"level":"ppv"}`, entities: []string{"restricted"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Legacy title"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 3, titles: map[string]string{"en": "One version"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
	)
	if err := f.entities.AddMemberships(f.ctx, []entities.Membership{{EntityID: entities.EntityID(f.ids["restricted"]), ContentID: f.cid(4), VersionID: f.vid(4, 0)}}); err != nil {
		t.Fatal(err)
	}
	f.exec(`DELETE FROM {s}.content_text WHERE content_id=$1`, f.cid(4))
	f.exec(`UPDATE {s}.content_items SET original_title='原題' WHERE id=$1`, f.cid(4))
	f.exec(`UPDATE {s}.content_version_text SET title='Hidden edition' WHERE version_id=$1`, f.vid(4, 0))
	f.exec(`INSERT INTO {s}.content_version_text(tenant_id,version_id,language,title) VALUES($1,$2,'ja','Allowed edition') ON CONFLICT(tenant_id,version_id,language) DO UPDATE SET title=EXCLUDED.title`, tenant, f.vid(4, 1))
	blockedVersion, _ := access.VersionKey(tenant, f.cid(4), f.vid(4, 0))
	b.held[blockedVersion.String()] = true
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
	if !got[f.cid(1)].Whole || len(got[f.cid(1)].VersionIDs) != 2 {
		t.Fatalf("whole purchase=%+v", got[f.cid(1)])
	}
	if got[f.cid(3)].Whole || len(got[f.cid(3)].VersionIDs) != 1 || got[f.cid(3)].VersionIDs[0] != f.vid(3, 1) {
		t.Fatalf("version purchase=%+v", got[f.cid(3)])
	}
	if _, exists := got[f.cid(2)]; exists {
		t.Fatal("hard hidden purchase leaked")
	}
	if _, exists := got[f.cid(4)]; exists {
		t.Fatal("unowned allowed sibling exposed a blocked purchased edition")
	}
	allowedVersion, _ := access.VersionKey(tenant, f.cid(4), f.vid(4, 1))
	b.held[allowedVersion.String()] = true
	if status, _ := f.call(http.MethodGet, "clean.test", "reader", "/me/library?language=en", nil, &page); status != http.StatusOK {
		t.Fatalf("updated library status=%d", status)
	}
	found := false
	for _, it := range page.Items {
		if it.ContentID == f.cid(4) {
			found = true
			if it.Title != "Allowed edition" || len(it.VersionIDs) != 1 || it.VersionIDs[0] != f.vid(4, 1) {
				t.Fatalf("edition eligibility/title=%+v", it)
			}
		}
	}
	if !found {
		t.Fatal("eligible purchased edition missing")
	}
	f.exec(`UPDATE {s}.content_version_text SET title='' WHERE version_id=$1`, f.vid(4, 1))
	if status, _ := f.call(http.MethodGet, "clean.test", "reader", "/me/library?language=en", nil, &page); status != http.StatusOK {
		t.Fatalf("fallback library status=%d", status)
	}
	for _, it := range page.Items {
		if it.ContentID == f.cid(4) && it.Title != "原題" {
			t.Fatalf("original title fallback=%+v", it)
		}
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

func TestPurchasedLibraryDoesNotRevealUnreleasedEditions(t *testing.T) {
	b := &dateBilling{releaseBilling: releaseBilling{held: map[string]bool{}}, complete: true}
	cfg := siteConfig()
	k := cfg.Kinds["gallery"]
	k.TitlePolicy = kinds.TitleOriginalAndVersion
	cfg.Kinds["gallery"] = k
	f := newFixtureBilling(t, nil, "", cfg, b)
	f.svc.o.Now = func() time.Time { return f.clock }
	f.seed(item{n: 1, titles: map[string]string{"en": "Released edition"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}, future: true}}})
	drafts, err := f.cat.PutVersions(f.ctx, testauth.Actor(access.Actor{ID: "staff"}, staffOnly{}), []catalog.VersionPut{{ContentID: f.cid(1), VersionInput: catalog.VersionInput{Languages: []string{"en"}, State: catalog.StateDraft, Text: map[string]catalog.VersionText{"en": {Title: "Secret draft"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 {
		t.Fatal("missing draft fixture")
	}
	future := f.vid(1, 1)
	f.exec(`INSERT INTO {s}.content_version_text(tenant_id,version_id,language,title) VALUES($1,$2,'ja','Unreleased title') ON CONFLICT(tenant_id,version_id,language) DO UPDATE SET title=EXCLUDED.title`, tenant, future)
	whole, _ := access.ContentKey(tenant, f.cid(1))
	b.held[whole.String()] = true
	var page LibraryPage
	if status, _ := f.call(http.MethodGet, "main.test", "reader", "/me/library?language=ja", nil, &page); status != http.StatusOK {
		t.Fatalf("library status=%d", status)
	}
	if len(page.Items) != 1 || len(page.Items[0].VersionIDs) != 1 || page.Items[0].VersionIDs[0] != f.vid(1, 0) || page.Items[0].Title != "Released edition" {
		t.Fatalf("unreleased edition leaked: %+v", page)
	}
	// A released edition remains discoverable to its purchaser during soft
	// deletion; the same exception must not reveal its unreleased siblings.
	if err := f.cat.DeleteVersions(f.ctx, testauth.Actor(access.Actor{ID: "staff"}, staffOnly{}), []catalog.VersionKey{{ContentID: f.cid(1), VersionID: f.vid(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "reader", "/me/library", nil, &page); status != http.StatusOK || len(page.Items) != 1 || len(page.Items[0].VersionIDs) != 1 || page.Items[0].VersionIDs[0] != f.vid(1, 0) {
		t.Fatalf("released deleted purchase lost: %d %+v", status, page)
	}
	// A whole-content entitlement is not publication permission. Once the
	// scheduled edition's own release time arrives it joins the library.
	f.clock = f.now.Add(2 * time.Hour)
	if status, _ := f.call(http.MethodGet, "main.test", "reader", "/me/library?language=ja", nil, &page); status != http.StatusOK {
		t.Fatalf("released library status=%d", status)
	}
	if len(page.Items) != 1 || len(page.Items[0].VersionIDs) != 2 || page.Items[0].Title != "Unreleased title" {
		t.Fatalf("released edition missing: %+v", page)
	}
}
