package critical_test

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
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
