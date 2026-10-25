package critical_test

import (
	"maps"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

// Fixed site channels use the same records as creator channels, but enforce
// host ownership and free access in every public write path. Public reads wait
// for the ordinary durable publication worker.
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
	startPublicationRuntime(t, a)
	item, err = staff.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, staff, item.ID, item.Versions[0].ID)
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
	managed, err := staff.GetContent(t.Context(), importedID, ck.GetContentQuery{})
	must(t, err)
	awaitEditionPublication(t, a, staff, importedID, managed.Versions[0].ID)
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
	_, err = admin.PatchContentVersion(t.Context(), paid.ID, paid.Versions[0].ID, catalog.VersionInput{Access: &access.Rule{Level: access.Public}})
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

// A staff author's credit and immutable creator identity must not become a
// personal editing grant after the staff role is revoked.
func TestFixedSiteChannelStaffRevocation(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["official"] = kinds.EntityKind{Route: "official", Owner: "site"}
		kind := cfg.Content.Kinds["article"]
		kind.OwnerEntity = &kinds.EntityRef{Kind: "official", Slug: "blog"}
		kind.AccessLevels = []string{"public"}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	_, err := ck.MigrateFixedOwners(t.Context(), w.pool, a.cfg, []entities.EntityInput{{Kind: "official", Slug: "blog", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official blog"}}}})
	must(t, err)
	author, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(author.ID), w.writer)
	writer := a.client(token)
	input := catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Staff authored", Body: "Official text"}}}
	draft, err := writer.PostContents(t.Context(), input)
	must(t, err)
	published, err := writer.PostContents(t.Context(), input)
	must(t, err)
	startPublicationRuntime(t, a)
	published, err = writer.PostContentPublish(t.Context(), published.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, writer, published.ID, published.Versions[0].ID)
	if len(draft.Creators) != 1 || draft.Creators[0].UserID != author.ID {
		t.Fatalf("fixture lacks author credit: %+v", draft.Creators)
	}
	authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(author.ID), w.writer)
	_, err = writer.GetContent(t.Context(), draft.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	_, err = writer.PatchContent(t.Context(), published.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.Public}})
	requireError(t, err, 403, "forbidden")
	err = writer.DeleteContentVersion(t.Context(), published.ID, published.Versions[0].ID)
	requireError(t, err, 403, "forbidden")
	visible, err := writer.GetContent(t.Context(), published.ID, ck.GetContentQuery{})
	must(t, err)
	if visible.Editor || !visible.Decision.Allowed || visible.Body != "Official text" {
		t.Fatalf("revoked author did not become an ordinary public reader: %+v", visible)
	}
	// Personal account blocks must not become moderation grants over the site's channel.
	reader, readerToken := w.person(t, 0)
	_, err = writer.PutCommentBan(t.Context(), reader.ID, content.BanInput{Reason: "Personal block"})
	must(t, err)
	standing, err := writer.GetContentCanComment(t.Context(), published.ID, ck.GetContentCanCommentQuery{})
	must(t, err)
	for _, scope := range standing.BanScopes {
		if scope == content.BanOwner {
			t.Fatal("former site author acquired personal moderation over the official channel")
		}
	}
	postComment(t, a.client(readerToken), published.ID, "Channel moderation remains with staff")
}

// A fixed destination does not dictate which AuthKit roles may publish there.
func TestFixedSiteChannelAuthorizationScope(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["channel"] = kinds.EntityKind{Route: "channels", Owner: "site", AuthScope: true, Entitled: true}
		kind := cfg.Content.Kinds["article"]
		kind.OwnerEntity = &kinds.EntityRef{Kind: "channel", Slug: "blog"}
		kind.AccessLevels = []string{"public"}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	seeds := []entities.EntityInput{{Kind: "channel", Slug: "blog", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official channel"}}}}
	if _, err := ck.MigrateFixedOwners(t.Context(), w.pool, a.cfg, seeds); err == nil {
		t.Fatal("database-only migration provisioned an authorization scope")
	}
	channels, err := a.engine.Client().GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
	must(t, err)
	if len(channels.Entities) != 0 {
		t.Fatal("refused scope provisioning left a channel")
	}
	create := func(app *application) entities.Entity {
		made, err := app.engine.Client().PostEntities(t.Context(), seeds)
		must(t, err)
		if len(made) != 1 || made[0].AuthScope == "" || made[0].OwnerUserID != "" {
			t.Fatalf("site scope not provisioned through AuthKit: %+v", made)
		}
		return made[0]
	}
	channel, foreign := create(a), create(w.apps[1])
	scope, err := ckauth.EntityScopeID(*a.cfg.Scope, channel.AuthScope)
	must(t, err)
	foreignScope, err := ckauth.EntityScopeID(*w.apps[1].cfg.Scope, foreign.AuthScope)
	must(t, err)
	if scope == foreignScope {
		t.Fatal("separate apps share an authorization group")
	}
	_, err = ck.MigrateFixedOwners(t.Context(), w.pool, a.cfg, seeds)
	must(t, err)
	member, token := w.person(t, 0)
	writer := a.client(token)
	input := catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Scoped official post", Body: "Host roles decide who posts"}}}
	_, err = writer.PostContents(t.Context(), input)
	requireError(t, err, 403, "forbidden")
	authtest.GrantRole(t, w.tenant[0], iam.GroupByID(foreignScope), iam.UserSubject(member.ID), w.channelWriter)
	_, err = writer.PostContents(t.Context(), input)
	requireError(t, err, 403, "forbidden")
	authtest.GrantRole(t, w.tenant[0], iam.GroupByID(scope), iam.UserSubject(member.ID), w.channelWriter)
	post, err := writer.PostContents(t.Context(), input)
	must(t, err)
	if post.OwnerEntityID != string(channel.ID) {
		t.Fatal("channel-scoped writer escaped the fixed destination")
	}
	startPublicationRuntime(t, a)
	_, err = writer.PostContentPublish(t.Context(), post.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, writer, post.ID, post.Versions[0].ID)
	public, err := a.client("").GetContent(t.Context(), post.ID, ck.GetContentQuery{})
	must(t, err)
	if !public.Decision.Allowed || public.Body != input.Text["en"].Body {
		t.Fatal("channel entitlement metadata overrode public post policy")
	}
}
