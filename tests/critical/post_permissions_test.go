package critical_test

import (
	"context"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/media"
)

func TestPostContributorAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	alice, aliceToken := w.person(t, 0)
	bob, bobToken := w.person(t, 0)
	author, credited := a.client(aliceToken), a.client(bobToken)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.contributor)
	newPost := func(title, channel string) catalog.ContentInput {
		return catalog.ContentInput{Kind: "article", OwnerEntityID: channel, Text: map[string]catalog.Text{"en": {Title: title, Body: "Private contributor source"}}}
	}
	t.Run("CK-J03-S01_app_create_is_personal_and_credits_are_not_authority", func(t *testing.T) {
		input := newPost("Personal contributor draft", "")
		credits := []catalog.Creator{{UserID: bob.ID, Role: "author"}}
		input.Creators = &credits
		item, err := author.PostContents(t.Context(), input)
		must(t, err)
		if len(item.Creators) != 1 || item.Creators[0].UserID != bob.ID {
			t.Fatal("credit fixture was not stored")
		}
		own, err := author.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if own.Body != "Private contributor source" {
			t.Fatal("authenticated author cannot read own draft")
		}
		_, err = credited.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = credited.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Credit cannot overwrite", Body: "no"})
		requireError(t, err, 403, "forbidden")
		_, err = credited.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		requireError(t, err, 403, "forbidden")
		managed, err := credited.GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(managed) != 0 {
			t.Fatal("attribution exposed a management draft")
		}
		includeContents := true
		suggestions, err := credited.GetManageSearchSuggestions(t.Context(), ck.GetManageSearchSuggestionsQuery{Q: "Personal contributor", IncludeContents: &includeContents, ContentKinds: []string{"article"}})
		must(t, err)
		if len(suggestions.Contents) != 0 {
			t.Fatal("attribution exposed a management suggestion")
		}
		_, err = author.PutContentCreators(t.Context(), item.ID, catalog.CreatorsInput{Creators: []catalog.Creator{}})
		must(t, err)
		_, err = author.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Still owned", Body: "Ownership survives removing credits"})
		must(t, err)
		_, err = author.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		public, err := a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if public.Body != "Ownership survives removing credits" {
			t.Fatal("own-post publication failed")
		}
		_, err = w.apps[1].client(aliceToken).PostContents(t.Context(), newPost("Cross app", ""))
		requireError(t, err, 403, "forbidden")
		_, err = credited.PostContents(t.Context(), newPost("No create grant", ""))
		requireError(t, err, 403, "forbidden")
	})
	t.Run("CK-J03-S05_current_app_grants_control_own_draft_and_mutations", func(t *testing.T) {
		person, token := w.person(t, 0)
		caller := a.client(token)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
		item, err := caller.PostContents(t.Context(), newPost("Create-only submission", ""))
		must(t, err)
		if item.Editor {
			t.Fatal("create-only grant conferred editing")
		}
		_, err = caller.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Must refuse", Body: "no"})
		requireError(t, err, 403, "forbidden")
		_, err = caller.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		requireError(t, err, 403, "forbidden")
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.contributor)
		_, err = caller.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Current edit grant", Body: "yes"})
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
		own, err := caller.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if own.Editor || own.Body != "yes" {
			t.Fatal("create-only draft read was lost or retained write")
		}
		_, err = caller.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Revoked", Body: "no"})
		requireError(t, err, 403, "forbidden")
		authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
		_, err = caller.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		ownList, err := caller.GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(ownList) != 0 {
			t.Fatal("revoked author still has a management listing")
		}
		include := true
		suggestions, err := caller.GetManageSearchSuggestions(t.Context(), ck.GetManageSearchSuggestionsQuery{Q: "Current edit", IncludeContents: &include})
		must(t, err)
		if len(suggestions.Contents) != 0 {
			t.Fatal("revoked author still has a management suggestion")
		}
	})
	t.Run("CK-J03-S01_channel_contributor_cannot_edit_other_posts_or_channel", func(t *testing.T) {
		channelOwner, ownerToken, channel := ownedEntity(t, w, a, "Community")
		owner := a.client(ownerToken)
		existing, err := owner.PostContents(t.Context(), newPost("Owner private draft", string(channel.ID)))
		must(t, err)
		_, err = author.PostContents(t.Context(), newPost("App grant is not channel grant", string(channel.ID)))
		requireError(t, err, 403, "forbidden")
		scopeID, err := ckauth.EntityScopeID(*a.cfg.Scope, channel.AuthScope)
		must(t, err)
		scope := iam.GroupByID(scopeID)
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(alice.ID), w.channelContributor)
		mine, err := author.GetMeEntities(t.Context(), ck.GetMeEntitiesQuery{})
		must(t, err)
		if len(mine.Entities) != 1 || mine.Entities[0].ID != channel.ID || !mine.Entities[0].CanPost || !mine.Entities[0].CanEditOwn || mine.Entities[0].Writer || mine.Entities[0].Editor || mine.Entities[0].Moderator {
			t.Fatalf("contributor channel capabilities wrong: %+v", mine.Entities)
		}
		own, err := author.PostContents(t.Context(), newPost("Contributor post", string(channel.ID)))
		must(t, err)
		_, err = author.PutContentText(t.Context(), own.ID, "en", catalog.Text{Title: "Own edit", Body: "Only this post"})
		must(t, err)
		_, err = author.GetContent(t.Context(), existing.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = author.PutContentText(t.Context(), existing.ID, "en", catalog.Text{Title: "Cannot edit owner", Body: "no"})
		requireError(t, err, 403, "forbidden")
		slug := "cannot-manage-channel"
		_, err = author.PatchEntity(t.Context(), string(channel.ID), entities.EntityUpdate{Slug: &slug})
		requireError(t, err, 403, "forbidden")
		_, _, foreignChannel := ownedEntity(t, w, w.apps[1], "Other app community")
		_, err = author.PostContents(t.Context(), newPost("Foreign app channel", string(foreignChannel.ID)))
		requireError(t, err, 403, "forbidden")
		_, err = author.PostContentPublish(t.Context(), own.ID, catalog.PublishInput{})
		must(t, err)
		draft, err := author.PostContents(t.Context(), newPost("Channel grant revocation", string(channel.ID)))
		must(t, err)
		authtest.RevokeRole(t, w.tenant[0], scope, iam.UserSubject(alice.ID), w.channelContributor)
		_, err = author.GetContent(t.Context(), draft.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = author.PutContentText(t.Context(), own.ID, "en", catalog.Text{Title: "Cannot use app grant here", Body: "no"})
		requireError(t, err, 403, "forbidden")
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(bob.ID), w.channelWriter)
		_, err = credited.PutContentText(t.Context(), existing.ID, "en", catalog.Text{Title: "Team editor still works", Body: "Team edit"})
		must(t, err)
		persona, err := w.auth.Persona("channel")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(bob.ID), persona.OwnerRole())
		authtest.RevokeRole(t, w.tenant[0], scope, iam.UserSubject(channelOwner.ID), persona.OwnerRole())
		_, err = owner.GetContent(t.Context(), existing.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = owner.PostContents(t.Context(), newPost("Owner metadata cannot bypass revocation", string(channel.ID)))
		requireError(t, err, 403, "forbidden")
		mine, err = owner.GetMeEntities(t.Context(), ck.GetMeEntitiesQuery{})
		must(t, err)
		if len(mine.Entities) != 0 {
			t.Fatalf("revoked owner metadata still grants channel capabilities: %+v", mine.Entities)
		}

	})
}

// Presigning is a real media mutation, not just an editor flag in a JSON response.
func TestPostContributorMediaRevocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	person, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.contributor)
	caller := a.client(token)
	ref := galleryRef(t, caller, "Own media grant")
	payload := imageBytes(t)
	request := media.PresignBody{Ref: ref, Path: "pages/one.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)}
	_, err := caller.PostMediaUploadPresign(t.Context(), request)
	must(t, err)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
	_, err = caller.PostMediaUploadPresign(t.Context(), request)
	requireError(t, err, 403, "forbidden")
	own, err := caller.GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
	must(t, err)
	if own.Editor {
		t.Fatal("create-only account retained media editorship")
	}
	authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
	_, err = caller.GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
}

// A community can belong to the site while AuthKit groups delegate its team.
func TestSiteChannelDelegatedCapabilities(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		kind := cfg.Content.EntityKinds["channel"]
		kind.Owner = "site"
		kind.MaxPerOwner = 0
		cfg.Content.EntityKinds["channel"] = kind
	})
	a := w.apps[0]
	attachMedia(t, w, a)
	rows, err := a.engine.Client().PostEntities(t.Context(), []entities.EntityInput{{Kind: "channel", Slug: "official", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official community"}}}})
	must(t, err)
	channel := rows[0]
	if channel.OwnerUserID != "" || channel.AuthScope == "" {
		t.Fatal("site channel fixture has wrong ownership")
	}
	id, err := ckauth.EntityScopeID(*a.cfg.Scope, channel.AuthScope)
	must(t, err)
	group := iam.GroupByID(id)
	manager, token := w.person(t, 0)
	client := a.client(token)
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelManager)
	_, err = client.PatchEntity(t.Context(), string(channel.ID), entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Renamed by manager"}}})
	must(t, err)
	_, err = client.PostEntities(t.Context(), []entities.EntityInput{{Kind: "channel", Slug: "not-authorized"}})
	requireError(t, err, 403, "forbidden")
	payload := imageBytes(t)
	ref := media.RefBody{Kind: "channel", ID: string(channel.ID)}
	presign := media.PresignBody{Ref: ref, Path: "cover/new.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)}
	_, err = client.PostMediaUploadPresign(t.Context(), presign)
	must(t, err)
	authtest.RevokeRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelManager)
	_, err = client.PostMediaUploadPresign(t.Context(), presign)
	requireError(t, err, 403, "forbidden")
	_, err = client.PatchEntity(t.Context(), string(channel.ID), entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Revoked manager"}}})
	requireError(t, err, 403, "forbidden")
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelWriter)
	input := catalog.ContentInput{Kind: "article", OwnerEntityID: string(channel.ID), Text: map[string]catalog.Text{"en": {Title: "Official post", Body: "Public"}}}
	item, err := client.PostContents(t.Context(), input)
	must(t, err)
	_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	_, commentToken := w.person(t, 0)
	comment := postComment(t, a.client(commentToken), item.ID, "Moderated site channel")
	moderator, moderatorToken := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(moderator.ID), w.channelModerator)
	must(t, a.client(moderatorToken).PostCommentHold(t.Context(), comment.ID))
	authtest.RevokeRole(t, w.tenant[0], group, iam.UserSubject(moderator.ID), w.channelModerator)
	err = a.client(moderatorToken).PostCommentHold(t.Context(), comment.ID)
	requireError(t, err, 403, "forbidden")
}

// Offline imports must not turn a not-yet-provisioned authorization scope into
// the intentionally unscoped direct-owner model.
func TestPendingChannelScopeHasNoAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine) // no engine workers until the final phase.
	owner := authtest.NewUser(t, a.tenant)
	owner.TOTP = authtest.EnrollTOTP(t, a.tenant, owner)
	token := authtest.SignIn(t, a.tenant, owner).AccessToken
	caller := a.client(token)
	id := contentref.NewID()
	result, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "pending-channel-scope", ProvisionScopes: true, Entities: []importer.EntityRecord{{ExternalID: "pending", ID: id, Kind: "channel", Slug: "pending-scope", OwnerUserID: owner.ID}}})
	must(t, err)
	if len(result.Failed) != 0 {
		t.Fatalf("pending scope fixture: %+v", result.Failed)
	}
	channel, err := a.engine.Client().GetManageEntity(t.Context(), id, ck.GetManageEntityQuery{})
	must(t, err)
	if channel.AuthScope != "" {
		t.Fatal("fixture processed scope before the checks")
	}
	input := catalog.ContentInput{Kind: "article", OwnerEntityID: id, Text: map[string]catalog.Text{"en": {Title: "Pending channel", Body: "Public content remains public"}}}
	item, err := a.engine.Client().PostContents(t.Context(), input)
	must(t, err)
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	gallery, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", OwnerEntityID: id, Text: map[string]catalog.Text{"en": {Title: "Pending gallery"}}})
	must(t, err)
	_, commentToken := w.person(t, 0)
	comment := postComment(t, a.client(commentToken), item.ID, "Not moderated by metadata")
	_, err = caller.PostContents(t.Context(), input)
	requireError(t, err, 403, "forbidden")
	_, err = caller.PutContentText(t.Context(), item.ID, "en", catalog.Text{Title: "Forbidden", Body: "no"})
	requireError(t, err, 403, "forbidden")
	update := entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Team updated"}}}
	_, err = caller.PatchEntity(t.Context(), id, update)
	requireError(t, err, 403, "forbidden")
	err = caller.PostCommentHold(t.Context(), comment.ID)
	requireError(t, err, 403, "forbidden")
	payload := imageBytes(t)
	requests := []media.PresignBody{
		{Ref: media.RefBody{Kind: "channel", ID: id}, Path: "cover/probe.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)},
		{Ref: media.RefBody{Kind: "gallery", ID: gallery.ID, Version: gallery.Versions[0].ID}, Path: "pages/probe.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)},
	}
	for _, request := range requests {
		_, err = caller.PostMediaUploadPresign(t.Context(), request)
		requireError(t, err, 403, "forbidden")
	}
	mine, err := caller.GetMeEntities(t.Context(), ck.GetMeEntitiesQuery{})
	must(t, err)
	if len(mine.Entities) != 0 {
		t.Fatal("pending scope advertised owner capabilities")
	}
	must(t, a.engine.Start(context.Background()))
	waitFor(t, func() bool {
		current, err := a.engine.Client().GetManageEntity(t.Context(), id, ck.GetManageEntityQuery{})
		must(t, err)
		return current.AuthScope != ""
	})
	_, err = caller.PostContents(t.Context(), input)
	must(t, err)
	_, err = caller.PatchEntity(t.Context(), id, update)
	must(t, err)
	must(t, caller.PostCommentHold(t.Context(), comment.ID))
	for _, request := range requests {
		_, err = caller.PostMediaUploadPresign(t.Context(), request)
		must(t, err)
	}
}
