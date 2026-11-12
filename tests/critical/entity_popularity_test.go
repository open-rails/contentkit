package critical_test

import (
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

func TestEntityPopularity(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", Popular: &kinds.EntityPopularity{ContentKind: "article", Bucket: "week"}}
		cfg.Content.EntityKinds["label"] = kinds.EntityKind{Route: "labels"}
		article := cfg.Content.Kinds["article"]
		article.Entities = append(article.Entities, "tag", "label")
		article.Popularity = []kinds.PopularityBucket{{ID: "week", Days: 7}}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	configureDiscoverySignals(t, w, a)
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	admin := a.engine.Client()
	tag := func(slug string) entities.EntityID {
		created, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: slug, Names: []entities.Name{{Language: "en", Kind: "name", Name: slug}}}})
		must(t, err)
		return created[0].ID
	}
	solo, pair, quiet, unused := tag("solo"), tag("pair"), tag("quiet"), tag("unused")
	// A finally prepared day the refresh reuses: pair's two articles together
	// outscore solo's one; quiet's articles have no readers.
	tenant, day := a.cfg.Scope.AppID.String(), w.clock.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	for i, s := range []struct {
		tag     entities.EntityID
		readers uint64
	}{{solo, 30}, {pair, 20}, {pair, 20}, {quiet, 0}, {quiet, 0}, {quiet, 0}} {
		item := publishedArticle(t, a, "Popular tag article "+string(rune('a'+i)))
		_, err := admin.PutContentEntitiesShared(t.Context(), item.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: s.tag}}}, ck.PutContentEntitiesSharedQuery{})
		must(t, err)
		if s.readers > 0 {
			must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily (tenant,content_kind,domain,language,day,content_id,readers,eligible) VALUES(?,?,?,?,?,?,?,?)`,
				tenant, "article", "", "", day, item.ID, s.readers, uint64(100)))
		}
	}
	must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily_prepared (tenant,content_kind,domain,language,day,computed_at) VALUES(?,?,?,?,?,?)`,
		tenant, "article", "", "", day, day.AddDate(0, 0, 3)))
	refreshPopularRankings(t, w)
	late := tag("late")

	first, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 3})
	must(t, err)
	if ids := entityIDs(first.Entities); len(ids) != 3 || ids[0] != pair || ids[1] != solo || ids[2] != quiet || first.NextCursor == "" {
		t.Fatalf("first page = %v; want pair, solo, then quiet by content count", ids)
	}
	next, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 3, Cursor: first.NextCursor})
	must(t, err)
	if ids := entityIDs(next.Entities); len(ids) != 2 || ids[0] != unused || ids[1] != late || next.NextCursor != "" {
		t.Fatalf("next page = %v; want unused, then the tag created since the ranking", ids)
	}
	numbered, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Sort: "popular", Limit: 2, Page: 2})
	must(t, err)
	if ids := entityIDs(numbered.Entities); len(ids) != 2 || ids[0] != quiet || ids[1] != unused || numbered.Total == nil || *numbered.Total != 5 {
		t.Fatalf("page 2 = %v of %v; want quiet, unused of 5", ids, numbered.Total)
	}
	_, err = admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "label", Sort: "popular"})
	requireError(t, err, 400, "invalid_request")
	_, err = admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Sort: "popular"})
	requireError(t, err, 400, "invalid_request")
}

func entityIDs(cards []entities.EntityCard) []entities.EntityID {
	ids := make([]entities.EntityID, len(cards))
	for i, c := range cards {
		ids[i] = c.ID
	}
	return ids
}
