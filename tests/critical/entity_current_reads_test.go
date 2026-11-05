package critical_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

func TestEntityListContentKindEligibility(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
	})
	a := w.apps[0]
	admin, public := a.engine.Client(), a.client("")
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{
		{Kind: "tag", Slug: "owner", Names: []entities.Name{{Language: "en", Kind: "name", Name: "A Owner"}}},
		{Kind: "tag", Slug: "member", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Z Member"}}},
		{Kind: "tag", Slug: "ineligible"},
		{Kind: "tag", Slug: "empty"},
	})
	must(t, err)
	owner, member, ineligible, empty := tags[0].ID, tags[1].ID, tags[2].ID, tags[3].ID
	// Seed the read projection directly so duplicate editions and a tag linked
	// only through owner_entity_id exercise the exact facet membership contract.
	seed := func(kind, language string, members []string, owner entities.EntityID) {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: kind, Versions: []catalog.VersionInput{
			{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "English"}}},
			{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"de": {Title: "German"}}},
		}})
		must(t, err)
		result, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema, "content_facets"}.Sanitize()+`
 SET live_from=now()-interval '1 hour', published_at=now()-interval '1 hour', access_level='public',
 languages=ARRAY[$3]::text[], entity_ids=$4::text[], owner_entity_id=nullif($5, '')
 WHERE tenant_id=$1 AND content_id=$2`, a.cfg.Scope.AppID.String(), item.ID, language, members, string(owner))
		must(t, err)
		if result.RowsAffected() != 2 {
			t.Fatalf("seeded %d facets, want two editions", result.RowsAffected())
		}
	}
	seed("gallery", "en", []string{string(member), string(member)}, owner)
	seed("gallery", "en", []string{string(member)}, member) // owner and member count once.
	seed("article", "en", []string{string(ineligible)}, "")
	seed("gallery", "de", []string{string(ineligible)}, "")
	for _, sort := range []string{"count", "name"} {
		t.Run(sort, func(t *testing.T) {
			want := []entities.EntityID{member, owner}
			if sort == "name" {
				want = []entities.EntityID{owner, member}
			}
			counts := map[entities.EntityID]int{owner: 1, member: 2}
			query := ck.GetEntitiesQuery{Kind: "tag", ContentKind: "gallery", Language: "en", Sort: sort, Page: 1, Limit: 1}
			first, err := public.GetEntities(t.Context(), query)
			must(t, err)
			if len(first.Entities) != 1 || first.Entities[0].ID != want[0] || first.Entities[0].Count != counts[want[0]] || first.Total == nil || *first.Total != 2 || first.NextCursor == "" {
				t.Fatalf("first page: %+v; want %s and total 2", first, want[0])
			}
			query.Page = 2
			second, err := public.GetEntities(t.Context(), query)
			must(t, err)
			if len(second.Entities) != 1 || second.Entities[0].ID != want[1] || second.Entities[0].Count != counts[want[1]] || second.Total == nil || *second.Total != 2 || second.NextCursor != "" {
				t.Fatalf("second page: %+v; want %s and total 2", second, want[1])
			}
			query.Page, query.Cursor = 0, first.NextCursor
			next, err := public.GetEntities(t.Context(), query)
			must(t, err)
			if len(next.Entities) != 1 || next.Entities[0].ID != want[1] || next.Entities[0].Count != counts[want[1]] || next.Total != nil || next.NextCursor != "" {
				t.Fatalf("cursor page: %+v; want %s", next, want[1])
			}
			for _, filter := range []struct {
				kind, language string
				counts         map[entities.EntityID]int
			}{
				{"gallery", "", map[entities.EntityID]int{owner: 1, member: 2, ineligible: 1}},
				{"", "en", map[entities.EntityID]int{owner: 1, member: 2, ineligible: 1, empty: 0}},
			} {
				page, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: filter.kind, Language: filter.language, Sort: sort, Page: 1})
				must(t, err)
				if len(page.Entities) != len(filter.counts) || page.Total == nil || *page.Total != len(filter.counts) {
					t.Fatalf("kind=%q language=%q: %+v", filter.kind, filter.language, page)
				}
				for _, card := range page.Entities {
					if count, ok := filter.counts[card.ID]; !ok || card.Count != count {
						t.Fatalf("kind=%q language=%q: unexpected card %+v", filter.kind, filter.language, card)
					}
				}
			}
		})
	}
}

// Only publication runs initially. Counts and covers must follow membership
// edits with every worker stopped, without an entity projection job.
func TestEntityCountsAndCoversWithoutWorkers(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", Parent: "tag"}
		kind := cfg.Content.Kinds["article"]
		kind.Entities = []string{"tag"}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	admin, public := a.engine.Client(), a.client("")
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "parent", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Parent"}}}})
	must(t, err)
	parent := tags[0]
	tags, err = admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "child", ParentID: parent.ID, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Child"}}}})
	must(t, err)
	child := tags[0]
	link := func(id string, entityIDs ...entities.EntityID) {
		links := []entities.Membership{}
		for _, entityID := range entityIDs {
			links = append(links, entities.Membership{EntityID: entityID})
		}
		_, err := admin.PutContentEntitiesShared(t.Context(), id, entities.ContentEntities{Memberships: links}, ck.PutContentEntitiesSharedQuery{})
		must(t, err)
	}
	check := func(want int, cover string) {
		t.Helper()
		for _, sort := range []string{"name", "count", "newest"} {
			page, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article", Language: "en", Sort: sort, Page: 1})
			must(t, err)
			if listed := 2 * want; len(page.Entities) != listed || page.Total == nil || *page.Total != listed {
				t.Fatalf("directory %s: %+v; want %d entities", sort, page, listed)
			}
			german, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article", Language: "de", Sort: sort, Page: 1})
			must(t, err)
			if len(german.Entities) != 0 || german.Total == nil || *german.Total != 0 {
				t.Fatalf("directory %s listed tags without German articles: %+v", sort, german)
			}
			for _, card := range page.Entities {
				if card.Count != want || cover == "" && card.Cover != nil || cover != "" && (card.Cover == nil || card.Cover.ContentID != cover) {
					t.Fatalf("directory %s count/cover without projection work: %+v; want count=%d cover=%s", sort, card, want, cover)
				}
			}
			lettered, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", ContentKind: "article", Language: "en", Sort: sort, Letter: "c", Page: 1})
			must(t, err)
			if want == 0 {
				if len(lettered.Entities) != 0 {
					t.Fatalf("directory %s letter c listed an empty tag: %+v", sort, lettered)
				}
			} else if len(lettered.Entities) != 1 || lettered.Entities[0].ID != child.ID || lettered.Total == nil || *lettered.Total != 1 || lettered.Entities[0].Count != want {
				t.Fatalf("directory %s letter c: %+v; want child with count=%d and total 1", sort, lettered, want)
			}
		}
		detail, err := public.GetEntity(t.Context(), string(child.ID), ck.GetEntityQuery{Language: "en"})
		must(t, err)
		for _, count := range detail.Counts {
			if count.ContentKind == "article" && count.Count != want {
				t.Fatalf("detail count diverges from directory: %+v", detail.Counts)
			}
		}
	}
	check(0, "")
	fleet := publicationOnlyFleet(t, a)
	one := publishedArticle(t, a, "Already published")
	must(t, fleet.Stop(t.Context()))
	link(one.ID, child.ID, parent.ID) // inherited and explicit parent count once.
	check(1, one.ID)
	future, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Future", Body: "Not released"}}}}})
	must(t, err)
	link(future.ID, child.ID)
	at := time.Now().Add(time.Second)
	_, err = admin.PostContentPublish(t.Context(), future.ID, catalog.PublishInput{PublishAt: &at})
	must(t, err)
	check(1, one.ID)
	w.clock.set(at.Add(time.Second))
	waitFor(t, func() bool {
		var due bool
		must(t, w.pool.QueryRow(t.Context(), "SELECT now()>=$1", at).Scan(&due))
		return due
	})
	check(1, one.ID) // The clock alone cannot include the uncompleted release.
	link(one.ID)
	check(0, "")
	link(one.ID, child.ID)
	check(1, one.ID)
	must(t, admin.DeleteContentVersion(t.Context(), one.ID, one.Versions[0].ID))
	check(0, "")
	other, err := w.apps[1].client("").GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag"})
	must(t, err)
	if len(other.Entities) != 0 {
		t.Fatal("entity reads crossed the app boundary")
	}
}
