package critical_test

import (
	"maps"
	"testing"

	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

// Fixed site channels use the same records as creator channels, but enforce
// host ownership and free access in every public write path, without workers.
func TestFixedSiteChannelPolicy(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["official"] = kinds.EntityKind{Route: "official", Owner: "site"}
		article := cfg.Content.Kinds["article"]
		article.OwnerEntity = &kinds.EntityRef{Kind: "official", Slug: "blog"}
		article.AccessLevels = []string{"public"}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Official blog editor", Role: persona.OwnerRole()})
	must(t, err)
	staff, public := a.client(key.Secret), a.client("")
	input := catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Official news", Body: "A free public announcement"}}}
	_, err = staff.PostContents(t.Context(), input)
	requireError(t, err, 503, "unavailable")
	createChannel := func(app *application, slug string) entities.Entity {
		result, err := app.engine.Client().PostEntities(t.Context(), []entities.EntityInput{{Kind: "official", Slug: slug, Names: []entities.Name{{Language: "en", Kind: "name", Name: slug}}}})
		must(t, err)
		if len(result) != 1 {
			t.Fatalf("missing channel: %+v", result)
		}
		return result[0]
	}
	foreign := createChannel(w.apps[1], "blog")
	_, err = staff.PostContents(t.Context(), input)
	requireError(t, err, 503, "unavailable") // another app's identical slug is not a seed.
	official := createChannel(a, "blog")
	other := createChannel(a, "other")
	for _, owner := range []string{string(other.ID), string(foreign.ID)} {
		wrong := input
		wrong.OwnerEntityID = owner
		_, err := staff.PostContents(t.Context(), wrong)
		requireError(t, err, 400, "invalid_request")
	}
	_, ordinaryToken := w.person(t, 0)
	_, err = a.client(ordinaryToken).PostContents(t.Context(), input)
	requireError(t, err, 403, "forbidden")
	item, err := staff.PostContents(t.Context(), input)
	must(t, err)
	if item.OwnerEntityID != string(official.ID) {
		t.Fatalf("wrong fixed owner: %+v", item)
	}
	item, err = staff.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	got, err := public.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	must(t, err)
	if got.OwnerEntityID != string(official.ID) || !got.Decision.Allowed || got.Body != "A free public announcement" {
		t.Fatalf("official post not public: %+v", got)
	}
	restricted := input
	restricted.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}
	_, err = staff.PostContents(t.Context(), restricted)
	requireError(t, err, 400, "invalid_request")
	_, err = staff.PostContentVersions(t.Context(), item.ID, catalog.VersionInput{Languages: []string{"de"}, Access: restricted.Access})
	requireError(t, err, 400, "invalid_request")
	batch := importer.Batch{Source: "fixed-channel-test", Contents: []importer.ContentRecord{
		{ExternalID: "valid", Kind: "article", State: catalog.StatePublished, Text: input.Text},
		{ExternalID: "wrong-owner", Kind: "article", State: catalog.StatePublished, Text: input.Text, OwnerEntity: &importer.Ref{ID: string(other.ID)}},
		{ExternalID: "paid", Kind: "article", State: catalog.StatePublished, Text: input.Text, Access: restricted.Access},
	}}
	imported, err := staff.PostImport(t.Context(), batch)
	must(t, err)
	if imported.Written["contents"] != 1 || len(imported.Failed) != 2 {
		t.Fatalf("import bypassed policy: %+v", imported)
	}
	importedID := imported.IDs[importer.RecordContent]["valid"]
	if importedID == "" {
		t.Fatalf("import omitted ID: %+v", imported)
	}
	got, err = public.GetContent(t.Context(), importedID, ck.GetContentQuery{})
	must(t, err)
	if got.OwnerEntityID != string(official.ID) || !got.Decision.Allowed {
		t.Fatalf("import did not apply fixed free channel: %+v", got)
	}
}

func TestFixedSiteChannelMigration(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["official"] = kinds.EntityKind{Route: "official", Owner: "site"}
	})
	a := w.apps[0]
	admin := a.engine.Client()
	free := publishedArticle(t, a, "Existing free blog")
	paid, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}, Text: map[string]catalog.Text{"en": {Title: "Private must not silently become free", Body: "Retained policy"}}})
	must(t, err)
	foreign := publishedArticle(t, w.apps[1], "Another app")
	cfg := a.cfg
	cfg.Content.Kinds = maps.Clone(cfg.Content.Kinds)
	article := cfg.Content.Kinds["article"]
	article.OwnerEntity = &kinds.EntityRef{Kind: "official", Slug: "blog"}
	article.AccessLevels = []string{"public"}
	cfg.Content.Kinds["article"] = article
	seeds := []entities.EntityInput{{Kind: "official", Slug: "blog", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official blog"}}}}
	_, err = ck.MigrateFixedOwners(t.Context(), w.pool, cfg, seeds)
	if err == nil {
		t.Fatal("migration silently opened private content")
	}
	page, err := admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "official"})
	must(t, err)
	if len(page.Entities) != 0 {
		t.Fatal("rejected migration partially seeded the channel")
	}
	before, err := admin.GetContent(t.Context(), free.ID, ck.GetContentQuery{})
	must(t, err)
	if before.OwnerEntityID != "" {
		t.Fatal("rejected migration partially changed ownership")
	}
	_, err = admin.PatchContent(t.Context(), paid.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.Public}})
	must(t, err)
	changed, err := ck.MigrateFixedOwners(t.Context(), w.pool, cfg, seeds)
	must(t, err)
	if changed != 2 {
		t.Fatalf("backfilled %d posts; want2", changed)
	}
	page, err = admin.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "official"})
	must(t, err)
	if len(page.Entities) != 1 {
		t.Fatalf("fixed channel missing: %+v", page)
	}
	owner := string(page.Entities[0].ID)
	for _, id := range []string{free.ID, paid.ID} {
		got, err := admin.GetContent(t.Context(), id, ck.GetContentQuery{})
		must(t, err)
		if got.OwnerEntityID != owner {
			t.Fatalf("wrong migrated owner: %+v", got)
		}
	}
	other, err := w.apps[1].engine.Client().GetContent(t.Context(), foreign.ID, ck.GetContentQuery{})
	must(t, err)
	if other.OwnerEntityID != "" {
		t.Fatal("migration crossed app scope")
	}
	after, err := admin.GetContent(t.Context(), free.ID, ck.GetContentQuery{})
	must(t, err)
	changed, err = ck.MigrateFixedOwners(t.Context(), w.pool, cfg, seeds)
	must(t, err)
	again, err := admin.GetContent(t.Context(), free.ID, ck.GetContentQuery{})
	must(t, err)
	if changed != 0 || again.Revision != after.Revision || again.Body != after.Body {
		t.Fatal("idempotent migration changed retained content")
	}
}
