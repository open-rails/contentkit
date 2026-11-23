package critical_test

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

func TestEntityPublicReadsFollowHardDomainCatalogRules(t *testing.T) {
	fixed := false
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag"}
		cfg.Content.Kinds["article"] = article
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
			{Name: "main", Hosts: []string{"main.invalid"}},
			{Name: "restricted", Hosts: []string{"restricted-one.invalid", "restricted-two.invalid"}, Filters: []kinds.ContentFilter{
				{Entity: kinds.EntityRef{Kind: "tag", Slug: "restricted"}, Visible: false, UserToggle: &fixed},
			}},
			{Name: "required", Hosts: []string{"required.invalid"}, RequireEntities: map[string][]kinds.EntityRef{
				"article": {{Kind: "tag", Slug: "required"}},
			}},
			{Name: "missing", Hosts: []string{"missing.invalid"}, Filters: []kinds.ContentFilter{
				{Entity: kinds.EntityRef{Kind: "tag", Slug: "not-created"}, Visible: false, UserToggle: &fixed},
			}},
		}}
	})
	a := w.apps[0]
	admin := a.engine.Client()
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{
		{Kind: "tag", Slug: "shared", Names: []entities.Name{{Language: "en", Kind: "name", Name: "A Shared"}}},
		{Kind: "tag", Slug: "allowed", Names: []entities.Name{{Language: "en", Kind: "name", Name: "B Allowed"}}},
		{Kind: "tag", Slug: "hidden-only"}, {Kind: "tag", Slug: "restricted", Internal: true},
		{Kind: "tag", Slug: "required"}, {Kind: "tag", Slug: "other-kind"}, {Kind: "tag", Slug: "empty"},
	})
	must(t, err)
	shared, allowed, hidden, restricted, required, other := tags[0].ID, tags[1].ID, tags[2].ID, tags[3].ID, tags[4].ID, tags[5].ID
	seed := func(kind string, members ...entities.EntityID) string {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: kind, Versions: []catalog.VersionInput{
			{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Domain entity fixture"}}},
		}})
		must(t, err)
		ids := make([]string, len(members))
		for i, id := range members {
			ids[i] = string(id)
		}
		result, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema, "content_facets"}.Sanitize()+`
 SET live_from=now()-interval '1 hour', published_at=now()-interval '1 hour', access_level='public', entity_ids=$3::text[]
 WHERE tenant_id=$1 AND content_id=$2`, a.cfg.Scope.AppID.String(), item.ID, ids)
		must(t, err)
		if result.RowsAffected() != 1 {
			t.Fatalf("seeded %d facets, want 1", result.RowsAffected())
		}
		return item.ID
	}
	first := seed("article", shared, allowed, required)
	second := seed("article", shared, allowed)
	blocked := seed("article", shared, hidden, restricted)
	seed("article", hidden, restricted)
	seed("gallery", other)
	client := func(host string) *ck.Client {
		return ck.NewClient(a.server.URL+"/content", &http.Client{Transport: domainTransport{host: host}})
	}
	hiddenMain, err := client("main.invalid").GetEntity(t.Context(), string(hidden), ck.GetEntityQuery{})
	must(t, err)
	if hiddenMain.Code == "" {
		t.Fatal("fixture has no direct entity code")
	}
	for _, host := range []string{"restricted-one.invalid", "restricted-two.invalid"} {
		t.Run(host, func(t *testing.T) {
			public := client(host)
			for _, sort := range []string{"name", "count", "newest"} {
				for _, cursor := range []bool{false, true} {
					query := ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article", Sort: sort, Page: 1, Limit: 1}
					if cursor {
						query.Page = 0
					}
					seen := map[entities.EntityID]int{}
					for n := 0; n < 3; n++ {
						page, err := public.GetEntities(t.Context(), query)
						must(t, err)
						if len(page.Entities) != 1 {
							t.Fatalf("sort=%s cursor=%t: %+v", sort, cursor, page)
						}
						if !cursor && (page.Total == nil || *page.Total != 3) {
							t.Fatalf("incorrect domain total: %+v", page)
						}
						card := page.Entities[0]
						if _, duplicate := seen[card.ID]; duplicate {
							t.Fatal("duplicate entity across pages")
						}
						seen[card.ID] = card.Count
						if card.Cover != nil && card.Cover.ContentID == blocked {
							t.Fatal("automatic cover exposed excluded content")
						}
						if cursor {
							query.Cursor = page.NextCursor
						} else {
							query.Page++
						}
						if n < 2 && page.NextCursor == "" {
							t.Fatal("missing next cursor")
						}
						if n == 2 && page.NextCursor != "" {
							t.Fatal("extra hidden entity affected pagination")
						}
					}
					if len(seen) != 3 || seen[shared] != 2 || seen[allowed] != 2 || seen[required] != 1 {
						t.Fatalf("wrong allowed entities/counts: %v", seen)
					}
				}
			}
			_, err := public.GetEntity(t.Context(), string(hidden), ck.GetEntityQuery{})
			requireError(t, err, http.StatusNotFound, "not_found")
			_, err = public.GetCode(t.Context(), hiddenMain.Code, ck.GetCodeQuery{})
			requireError(t, err, http.StatusNotFound, "not_found")
			detail, err := public.GetEntity(t.Context(), string(shared), ck.GetEntityQuery{Language: "en"})
			must(t, err)
			foundCount := false
			for _, count := range detail.Counts {
				if count.ContentKind == "article" {
					foundCount = true
					if count.Count != 2 {
						t.Fatalf("detail leaked excluded count: %+v", detail.Counts)
					}
				}
			}
			if !foundCount {
				t.Fatal("detail lost visible article counts")
			}
			_, err = public.GetCode(t.Context(), detail.Code, ck.GetCodeQuery{})
			must(t, err)
			if detail.Cover != nil && detail.Cover.ContentID != first && detail.Cover.ContentID != second {
				t.Fatalf("detail exposed excluded automatic cover: %+v", detail.Cover)
			}
			allKinds, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Page: 1})
			must(t, err)
			if len(allKinds.Entities) != 4 {
				t.Fatalf("unscoped-kind read leaked hidden or empty tags: %+v", allKinds)
			}
			wantCounts := map[entities.EntityID]int{shared: 2, allowed: 2, required: 1, other: 1}
			for _, card := range allKinds.Entities {
				if count, ok := wantCounts[card.ID]; !ok || card.Count != count {
					t.Fatalf("wrong unscoped-kind entity/count: %+v", card)
				}
			}
		})
	}
	main, err := client("main.invalid").GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article", Page: 1})
	must(t, err)
	if len(main.Entities) != 4 {
		t.Fatalf("main catalog changed: %+v", main)
	}
	for _, card := range main.Entities {
		if card.ID == shared && card.Count != 3 {
			t.Fatalf("main count changed: %+v", card)
		}
	}
	managed, err := admin.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "tag"})
	must(t, err)
	if len(managed.Entities) != 7 {
		t.Fatalf("management lost classifications: %+v", managed)
	}
	missing, err := client("missing.invalid").GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article"})
	must(t, err)
	if len(missing.Entities) != 0 {
		t.Fatalf("missing hard marker did not fail closed: %+v", missing)
	}
	otherKind, err := client("missing.invalid").GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "gallery"})
	must(t, err)
	if len(otherKind.Entities) != 1 || otherKind.Entities[0].ID != other {
		t.Fatalf("article rule changed another kind: %+v", otherKind)
	}
	requiredPage, err := client("required.invalid").GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article"})
	must(t, err)
	if len(requiredPage.Entities) != 3 {
		t.Fatalf("required boundary ignored: %+v", requiredPage)
	}
	for _, card := range requiredPage.Entities {
		if card.Count != 1 {
			t.Fatalf("required count leaked: %+v", card)
		}
	}
}

func TestAdminLocationRules(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.GeoIPCityPath = "../../internal/locationpolicy/testdata/GeoIP2-City-Test.mmdb"
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"tag"}
		article.Versions.Hidden = false
		cfg.Content.Kinds["article"] = article
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{{Name: "main", Hosts: []string{"main.invalid"}}, {Name: "other", Hosts: []string{"other.invalid"}}}}
	})
	a := w.apps[0]
	admin := a.engine.Client()
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "location-tag", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Location tag"}}}})
	must(t, err)
	items := []string{}
	versionIDs := []string{}
	for i := 0; i < 3; i++ {
		versions := []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Location fixture"}}}}
		if i == 0 {
			versions = append([]catalog.VersionInput{{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"de": {Title: "Deutscher Titel"}}}}, versions...)
		} else if i == 1 {
			versions = []catalog.VersionInput{{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"de": {Title: "Fallback title"}}}}
		}
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: versions})
		must(t, err)
		ids := []string{}
		if i == 1 {
			ids = []string{string(tags[0].ID)}
		}
		_, err = w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema, "content_facets"}.Sanitize()+` SET live_from=now()-interval '1 hour',published_at=now()-interval '1 hour',access_level='public',entity_ids=$3 WHERE tenant_id=$1 AND content_id=$2`, a.cfg.Scope.AppID.String(), item.ID, ids)
		must(t, err)
		items = append(items, item.ID)
		versionIDs = append(versionIDs, item.Versions[0].ID)
	}
	blocked := a.engine.VisitorClient(netip.MustParseAddr("203.0.113.45"), "main.invalid")
	uncovered := a.engine.VisitorClient(netip.MustParseAddr("203.0.114.45"), "main.invalid")
	other := a.engine.VisitorClient(netip.MustParseAddr("203.0.113.45"), "other.invalid")
	for _, id := range items {
		_, err = blocked.GetContent(t.Context(), id, ck.GetContentQuery{})
		must(t, err)
	}
	create := func(in content.LocationRuleInput) content.LocationRule {
		t.Helper()
		r, err := admin.PostModerationLocationRules(t.Context(), in)
		must(t, err)
		return r
	}
	rule := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "203.0.113.45"}}, Target: "content", ContentIDs: items[:2], Domains: []string{"main"}, Enabled: true, Note: "item rule"})
	if rule.Locations[0].Network != "203.0.113.45/32" || rule.CreatedBy != "contentkit" || rule.UpdatedAt.IsZero() {
		t.Fatalf("invalid persisted rule: %+v", rule)
	}
	tagRule := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "203.0.113.0/24"}}, Target: "entities", EntityIDs: []string{string(tags[0].ID)}, Domains: []string{"main"}, Enabled: true})
	wantContentTargets := []content.LocationRuleTarget{{ID: items[0], Kind: "article", Title: "Location fixture"}, {ID: items[1], Kind: "article", Title: "Fallback title"}}
	wantEntityTargets := []content.LocationRuleTarget{{ID: string(tags[0].ID), Kind: "tag", Name: "Location tag"}}
	if !reflect.DeepEqual(rule.Targets, wantContentTargets) || !reflect.DeepEqual(tagRule.Targets, wantEntityTargets) {
		t.Fatalf("create targets: content=%+v entities=%+v", rule.Targets, tagRule.Targets)
	}
	rules, err := admin.GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	must(t, err)
	if !slices.Equal(rules.Domains, []string{"main", "other"}) || rules.Total != 2 || len(rules.Items) != 2 {
		t.Fatalf("rule page: %+v", rules)
	}
	for _, r := range rules.Items {
		want := wantContentTargets
		if r.ID == tagRule.ID {
			want = wantEntityTargets
		}
		if !reflect.DeepEqual(r.Targets, want) {
			t.Fatalf("list targets: %+v", r)
		}
	}
	for _, id := range items[:2] {
		_, err = blocked.GetContent(t.Context(), id, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = uncovered.GetContent(t.Context(), id, ck.GetContentQuery{})
		must(t, err)
		_, err = other.GetContent(t.Context(), id, ck.GetContentQuery{})
		must(t, err)
	}
	page, err := blocked.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
	must(t, err)
	assertCards(t, page, items[2])
	check, err := admin.GetModerationLocationRulesCheck(t.Context(), ck.GetModerationLocationRulesCheckQuery{Ip: "203.0.113.45"})
	must(t, err)
	if len(check.Domains["main"]) != 2 || len(check.Domains["other"]) != 0 || check.Location.Country != "" {
		t.Fatalf("incorrect check: %+v", check)
	}
	// Changes apply on the next request to the same client, with no TTL.
	note := "updated"
	updated, err := admin.PatchModerationLocationRule(t.Context(), rule.ID, content.LocationRuleUpdate{Note: &note})
	must(t, err)
	if !reflect.DeepEqual(updated.Targets, wantContentTargets) {
		t.Fatalf("update targets: %+v", updated)
	}
	enabled := false
	_, err = admin.PatchModerationLocationRule(t.Context(), rule.ID, content.LocationRuleUpdate{Enabled: &enabled})
	must(t, err)
	_, err = blocked.GetContent(t.Context(), items[0], ck.GetContentQuery{})
	must(t, err)
	enabled = true
	_, err = admin.PatchModerationLocationRule(t.Context(), rule.ID, content.LocationRuleUpdate{Enabled: &enabled})
	must(t, err)
	must(t, admin.DeleteModerationLocationRule(t.Context(), rule.ID))
	history, err := admin.GetModerationLocationRuleHistory(t.Context(), rule.ID, ck.GetModerationLocationRuleHistoryQuery{Limit: 100})
	must(t, err)
	want := []string{"delete", "enable", "disable", "update", "create"}
	if history.Total != len(want) || len(history.Items) != len(want) {
		t.Fatalf("missing transactional history: %+v", history)
	}
	for i, h := range history.Items {
		var snapshot content.LocationRule
		must(t, json.Unmarshal(h.Snapshot, &snapshot))
		var fields map[string]json.RawMessage
		must(t, json.Unmarshal(h.Snapshot, &fields))
		if _, exists := fields["targets"]; exists {
			t.Fatalf("history snapshot gained targets: %s", h.Snapshot)
		}
		if h.Action != want[i] || h.Actor != "contentkit" || snapshot.ID != rule.ID || h.At.IsZero() {
			t.Fatalf("bad history row: %+v", h)
		}
	}
	all, err := admin.GetModerationLocationRulesHistory(t.Context(), ck.GetModerationLocationRulesHistoryQuery{Limit: 1, Offset: 1})
	must(t, err)
	if all.Total != 6 || len(all.Items) != 1 {
		t.Fatalf("overall history pagination: %+v", all)
	}
	must(t, admin.DeleteModerationLocationRule(t.Context(), tagRule.ID))
	// Countries/subdivisions use the real published synthetic MaxMind fixture.
	geoRule := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}, {Subdivision: "GB-ENG"}}, Target: "site", Domains: []string{"main"}, Enabled: true})
	site, yes, err := a.engine.SiteBlock(t.Context(), netip.MustParseAddr("81.2.69.142"), "main.invalid")
	must(t, err)
	if !yes || site.Country != "GB" || site.Subdivision != "GB-ENG" || site.CountryName != "United Kingdom" || site.Kind != "country" || site.RuleID != geoRule.ID {
		t.Fatalf("whole-site match: %+v %t", site, yes)
	}
	_, yes, err = a.engine.SiteBlock(t.Context(), netip.MustParseAddr("81.2.69.142"), "other.invalid")
	must(t, err)
	if yes {
		t.Fatal("whole-site rule crossed domain")
	}
	_, yes, err = a.engine.SiteBlock(t.Context(), netip.MustParseAddr("10.0.0.1"), "main.invalid")
	must(t, err)
	if yes {
		t.Fatal("lookup miss matched country")
	}
	_, yes, err = w.apps[1].engine.SiteBlock(t.Context(), netip.MustParseAddr("81.2.69.142"), "main.invalid")
	must(t, err)
	if yes {
		t.Fatal("rule crossed tenant")
	}
	_, err = a.engine.VisitorClient(netip.MustParseAddr("81.2.69.142"), "main.invalid").GetContent(t.Context(), items[2], ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	must(t, admin.DeleteModerationLocationRule(t.Context(), geoRule.ID))
	// Raw staff status and event feeds retain every item despite location rules.
	metadataRule := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "127.0.0.0/8"}}, Target: "content", ContentIDs: items[:2], Domains: []string{"main"}, Enabled: true})
	statuses, err := admin.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: versionIDs})
	must(t, err)
	if len(statuses) != len(items) {
		t.Fatalf("location rule filtered staff status feed: %+v", statuses)
	}
	events, err := admin.GetEvents(t.Context(), ck.GetEventsQuery{Limit: 100})
	must(t, err)
	seen := map[string]bool{}
	for _, event := range events.Events {
		seen[event.ContentID] = true
	}
	for _, id := range items {
		if !seen[id] {
			t.Fatalf("location rule filtered %s from staff event feed", id)
		}
	}

	must(t, admin.DeleteModerationLocationRule(t.Context(), metadataRule.ID))
	// Staff administration stays reachable from a blocked staff IP.
	staffRule := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "127.0.0.0/8"}}, Target: "site", Domains: []string{"main"}, Enabled: true})
	_, err = admin.GetContent(t.Context(), items[2], ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	staffTargets := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "127.0.0.0/8"}}, Target: "content", ContentIDs: items[:2], Domains: []string{"main"}, Enabled: true})
	rules, err = admin.GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	must(t, err)
	for _, r := range rules.Items {
		if r.ID == staffTargets.ID && !reflect.DeepEqual(r.Targets, wantContentTargets) {
			t.Fatalf("admin location filtered target labels: %+v", r)
		}
	}
	updated, err = admin.PatchModerationLocationRule(t.Context(), staffTargets.ID, content.LocationRuleUpdate{Note: &note})
	must(t, err)
	if !reflect.DeepEqual(staffTargets.Targets, wantContentTargets) || !reflect.DeepEqual(updated.Targets, wantContentTargets) {
		t.Fatalf("admin location filtered mutation labels: create=%+v update=%+v", staffTargets, updated)
	}
	must(t, admin.DeleteModerationLocationRule(t.Context(), staffTargets.ID))
	managed, err := admin.GetManage(t.Context(), ck.GetManageQuery{})
	must(t, err)
	if len(managed) != len(items) {
		t.Fatalf("whole-site rule filtered staff management: %+v", managed)
	}
	statuses, err = admin.GetVersions(t.Context(), ck.GetVersionsQuery{Ids: versionIDs})
	must(t, err)
	if len(statuses) != len(items) {
		t.Fatalf("whole-site rule filtered staff statuses: %+v", statuses)
	}
	_, err = admin.GetCommentsAdmin(t.Context(), ck.GetCommentsAdminQuery{})
	must(t, err)
	_, err = admin.GetModerationHeld(t.Context(), ck.GetModerationHeldQuery{Kind: content.KindComment})
	must(t, err)
	must(t, admin.DeleteModerationLocationRule(t.Context(), staffRule.ID))
	_, token := w.person(t, 0)
	_, err = a.client(token).GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	requireError(t, err, 403, "forbidden")
	_, err = blocked.GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	requireError(t, err, 401, "unauthorized")
	invalid := content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "site", Domains: []string{"unknown"}, Enabled: true}
	_, err = admin.PostModerationLocationRules(t.Context(), invalid)
	requireError(t, err, 400, "invalid_request")
	invalid.Domains = []string{"main"}
	invalid.Locations = []content.LocationEntry{{Country: "GB", Network: "203.0.113.45"}}
	_, err = admin.PostModerationLocationRules(t.Context(), invalid)
	requireError(t, err, 400, "invalid_request")
	missingContent := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "content", ContentIDs: []string{items[2]}, Domains: []string{"main"}, Enabled: true})
	missingEntity := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "entities", EntityIDs: []string{string(tags[0].ID)}, Domains: []string{"main"}, Enabled: true})
	_, err = w.pool.Exec(t.Context(), `DELETE FROM `+pgx.Identifier{w.schema, "content_items"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), items[2])
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `DELETE FROM `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), string(tags[0].ID))
	must(t, err)
	rules, err = admin.GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	must(t, err)
	if rules.Total != 2 || len(rules.Items) != 2 {
		t.Fatalf("cascade removed rules: %+v", rules)
	}
	for _, r := range rules.Items {
		if r.ID != missingContent.ID && r.ID != missingEntity.ID {
			t.Fatalf("unexpected rule after cascade: %+v", r)
		}
		if !r.Enabled || r.ContentIDs == nil || r.EntityIDs == nil || r.Targets == nil || len(r.ContentIDs)+len(r.EntityIDs)+len(r.Targets) != 0 {
			t.Fatalf("deleted target retained ids or labels: %+v", r)
		}
	}
	must(t, admin.DeleteModerationLocationRule(t.Context(), missingContent.ID))
	must(t, admin.DeleteModerationLocationRule(t.Context(), missingEntity.ID))
	// A failed history insert must roll back its rule insert as well.
	qs := pgx.Identifier{w.schema}.Sanitize()
	_, err = w.pool.Exec(t.Context(), `CREATE FUNCTION `+qs+`.reject_location_history() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.snapshot->>'note'='reject-history' THEN RAISE EXCEPTION 'history unavailable'; END IF; RETURN NEW; END$$;
 CREATE TRIGGER reject_location_history BEFORE INSERT ON `+qs+`.content_location_rule_history FOR EACH ROW EXECUTE FUNCTION `+qs+`.reject_location_history()`)
	must(t, err)
	invalid.Locations = []content.LocationEntry{{Country: "GB"}}
	invalid.Note = "reject-history"
	_, err = admin.PostModerationLocationRules(t.Context(), invalid)
	requireError(t, err, 500, "internal_error")
	var remaining int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+qs+`.content_location_rules WHERE tenant_id=$1 AND note='reject-history'`, a.cfg.Scope.AppID.String()).Scan(&remaining))
	if remaining != 0 {
		t.Fatal("rule committed without history")
	}
}

func TestAdminLocationRuleDefaultDomains(t *testing.T) {
	w := newWorld(t)
	admin := w.apps[0].engine.Client()
	page, err := admin.GetModerationLocationRules(t.Context(), ck.GetModerationLocationRulesQuery{})
	must(t, err)
	if !slices.Equal(page.Domains, []string{"main"}) || len(page.Items) != 0 {
		t.Fatalf("unconfigured domains: %+v", page)
	}
	rule, err := admin.PostModerationLocationRules(t.Context(), content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "site", Domains: page.Domains})
	must(t, err)
	if rule.Targets == nil || len(rule.Targets) != 0 {
		t.Fatalf("site targets: %+v", rule)
	}
}
