package discovery

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
)

func TestInternalGroupOffersDoNotLeakThroughCards(t *testing.T) {
	cfg := siteConfig()
	cfg.GroupKinds["channel"] = kinds.GroupKind{Route: "channels", Sellable: true}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("channel", "private")
	hidden := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(f.ids["private"]), Internal: &hidden}}); err != nil {
		t.Fatal(err)
	}
	for i, rule := range []string{`{"level":"group","group_id":"` + f.ids["private"] + `"}`, `{"level":"group_ppv","group_id":"` + f.ids["private"] + `"}`, `{"level":"tier","tiers":["premium"]}`} {
		f.seed(item{n: i + 1, titles: map[string]string{"en": "Story"}, age: time.Hour, rule: rule, versions: []ver{{langs: []string{"en"}}}})
	}
	for _, user := range []string{"", "fan", "staff"} {
		var page ContentPage
		if status, _ := f.call(http.MethodGet, "main.test", user, "/contents?kind=gallery", nil, &page); status != http.StatusOK || len(page.Contents) != 3 {
			t.Fatalf("cards as %s: %d %+v", user, status, page)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), f.ids["private"]) {
			t.Fatalf("internal group leaked as %s: %s", user, raw)
		}
		for _, card := range page.Contents {
			switch card.ID {
			case f.cid(1):
				if card.Access.Allowed != (user == "staff") {
					t.Fatalf("group access changed: %+v", card.Access)
				}
				if user != "staff" && len(card.Access.Sell) != 2 {
					t.Fatalf("public purchase alternatives removed: %+v", card.Access)
				}
			case f.cid(2):
				if card.Access.Allowed != (user == "staff") || len(card.Access.Sell) != 0 {
					t.Fatalf("private prerequisite changed: %+v", card.Access)
				}
			case f.cid(3):
				if card.Access.Allowed != (user != "") {
					t.Fatalf("tier access changed: %+v", card.Access)
				}
				if user == "" && (len(card.Access.Sell) == 0 || card.Access.Sell[0] != "premium") {
					t.Fatalf("public tier hidden: %+v", card.Access)
				}
			}
		}
	}
}

func TestInternalEntitiesDoNotConsumeSavedItemsPages(t *testing.T) {
	f := seeded(t)
	hidden := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(f.ids["yaoi"]), Internal: &hidden}}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{f.ids["romance"], f.ids["yaoi"]} {
		f.exec(`INSERT INTO {s}.content_favorites (tenant_id,user_id,content_kind,content_id,value,revision,created_at) VALUES ($1,'reader','tag',$2,1,1,now()+$3::interval)`, tenant, id, []string{"1 minute", "2 minutes"}[i])
	}
	var page InteractionPage
	if status, _ := f.call(http.MethodGet, "main.test", "reader", "/me/favorites?target=entities&limit=1", nil, &page); status != http.StatusOK || len(page.Items) != 1 || page.Items[0].Group == nil || string(page.Items[0].Group.ID) != f.ids["romance"] || page.NextCursor != "" {
		t.Fatalf("internal favorite consumed page: %d %+v", status, page)
	}
}
