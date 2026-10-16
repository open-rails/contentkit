package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestInternalAccessGroupIsNotPublicPurchaseMetadata(t *testing.T) {
	f := newFixture(t, nil)
	group := f.group("channel", "private-channel")
	f.sell(group)
	hidden := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(group), Internal: &hidden}}); err != nil {
		t.Fatal(err)
	}
	in := gallery("Private group access", "en")
	in.OwnerGroupID = group
	in.Creators = &[]Creator{{UserID: "alice"}}
	in.Access = &access.Rule{Level: access.GroupLevel, Group: group}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	for _, who := range []string{anon, "staff", "alice"} {
		content := f.read(who, "main.test", id)[0]
		if content == nil {
			t.Fatalf("content missing for %s", who)
		}
		if content.Decision.Allowed != (who != anon) {
			t.Fatalf("redaction changed access for %s: %+v", who, content.Decision)
		}
		if content.Decision.Requires != "" || content.OwnerGroupID != "" || content.Access.Group != "" {
			t.Fatalf("private access metadata for %s: %+v", who, content)
		}
		if who == anon && (len(content.Decision.Sell) != 2 || len(content.Versions[0].Decision.Sell) != 2) {
			t.Fatalf("independent content and version offers lost: %+v", content.Decision)
		}
		raw, _ := json.Marshal(content)
		if strings.Contains(string(raw), group) {
			t.Fatalf("internal entity leaked in public content: %s", raw)
		}
	}
	req := testauth.As(httptest.NewRequest(http.MethodGet, "/manage/gallery", nil), f.as("staff"))
	req.Host = "main.test"
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), group) {
		t.Fatalf("management lost internal rule: %d %s", w.Code, w.Body)
	}
	req = testauth.As(httptest.NewRequest(http.MethodGet, "/manage/gallery", nil), f.as("alice"))
	req.Host = "main.test"
	w = httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), group) {
		t.Fatalf("content creator learned internal metadata: %d %s", w.Code, w.Body)
	}

}

func TestInternalOfferRedactionPreservesPublicAlternatives(t *testing.T) {
	f := newFixture(t, nil)
	group := f.group("channel", "private-alternative")
	hidden := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(group), Internal: &hidden}}); err != nil {
		t.Fatal(err)
	}
	key, _ := access.GroupKey(tenant, group)
	public, _ := access.TierKey("premium")
	content, _ := access.ContentKey(tenant, "01800000-0000-7000-8000-000000000001")
	items := map[string]*Content{
		"alternatives": {Decision: Decision{Sell: []string{key.String(), public.String(), content.String()}}},
		"required":     {Decision: Decision{Sell: []string{content.String()}, Requires: key.String()}},
		"allowed":      {Decision: Decision{Allowed: true, Sell: []string{key.String(), public.String()}}},
	}
	if err := f.s.redactInternalEntities(f.ctx, items); err != nil {
		t.Fatal(err)
	}
	if got := items["alternatives"].Decision; len(got.Sell) != 2 || got.Sell[0] != "premium" || got.Sell[1] != content.String() || got.Allowed {
		t.Fatalf("public alternatives changed: %+v", got)
	}
	if got := items["required"].Decision; len(got.Sell) != 0 || got.Requires != "" || got.Allowed {
		t.Fatalf("private prerequisite escaped: %+v", got)
	}
	if got := items["allowed"].Decision; !got.Allowed || len(got.Sell) != 1 || got.Sell[0] != "premium" {
		t.Fatalf("permitted access changed: %+v", got)
	}
}
