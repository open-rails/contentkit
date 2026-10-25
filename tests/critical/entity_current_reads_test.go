package critical_test

import (
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// The engine is never started: correctness cannot depend on a projection job.
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
			if len(page.Entities) != 2 || page.Total == nil || *page.Total != 2 {
				t.Fatalf("directory %s: %+v", sort, page)
			}
			for _, card := range page.Entities {
				if card.Count != want || cover == "" && card.Cover != nil || cover != "" && (card.Cover == nil || card.Cover.ContentID != cover) {
					t.Fatalf("directory %s count/cover before any worker: %+v; want count=%d cover=%s", sort, card, want, cover)
				}
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
	one := publishedArticle(t, a, "Already published")
	link(one.ID, child.ID, parent.ID) // inherited and explicit parent count once.
	check(1, one.ID)
	future, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Future", Body: "Not released"}}})
	must(t, err)
	link(future.ID, child.ID)
	at := time.Now().Add(time.Hour)
	_, err = admin.PostContentPublish(t.Context(), future.ID, catalog.PublishInput{PublishAt: &at})
	must(t, err)
	check(1, one.ID)
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
