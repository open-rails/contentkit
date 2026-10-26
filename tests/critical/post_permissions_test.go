package critical_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/media"
)

func TestPostContributorAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	startPublicationRuntime(t, a)
	alice, aliceToken := w.person(t, 0)
	bob, bobToken := w.person(t, 0)
	author, credited := a.client(aliceToken), a.client(bobToken)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.contributor)
	newPost := func(title, channel string) catalog.ContentInput {
		return catalog.ContentInput{Kind: "article", ChannelID: channel, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title, Body: "Private contributor source"}}}}}
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
		_, err = credited.PatchContentVersion(t.Context(), item.ID, item.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Credit cannot overwrite", Body: "no"}}})
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
		_, err = author.PatchContentVersion(t.Context(), item.ID, item.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Still owned", Body: "Ownership survives removing credits"}}})
		must(t, err)
		_, err = author.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, author, item.ID, item.Versions[0].ID)
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
		_, err = caller.PatchContentVersion(t.Context(), item.ID, item.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Must refuse", Body: "no"}}})
		requireError(t, err, 403, "forbidden")
		_, err = caller.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		requireError(t, err, 403, "forbidden")
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.contributor)
		_, err = caller.PatchContentVersion(t.Context(), item.ID, item.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Current edit grant", Body: "yes"}}})
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(person.ID), w.submitter)
		own, err := caller.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if own.Editor || own.Body != "yes" {
			t.Fatal("create-only draft read was lost or retained write")
		}
		_, err = caller.PatchContentVersion(t.Context(), item.ID, item.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Revoked", Body: "no"}}})
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
		channelOwner, ownerToken, channel := ownedChannel(t, w, a, "Community")
		owner := a.client(ownerToken)
		existing, err := owner.PostContents(t.Context(), newPost("Owner private draft", string(channel.ID)))
		must(t, err)
		_, err = author.PostContents(t.Context(), newPost("App grant is not channel grant", string(channel.ID)))
		requireError(t, err, 403, "forbidden")
		scopeID, err := ckauth.ChannelScopeID(*a.cfg.Scope, channel.AuthScope)
		must(t, err)
		scope := iam.GroupByID(scopeID)
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(alice.ID), w.channelContributor)
		mine, err := author.GetMeChannels(t.Context(), ck.GetMeChannelsQuery{})
		must(t, err)
		if len(mine.Channels) != 1 || mine.Channels[0].ID != channel.ID || !mine.Channels[0].CanPost || !mine.Channels[0].CanEditOwn || mine.Channels[0].Writer || mine.Channels[0].Editor || mine.Channels[0].Moderator {
			t.Fatalf("contributor channel capabilities wrong: %+v", mine.Channels)
		}
		own, err := author.PostContents(t.Context(), newPost("Contributor post", string(channel.ID)))
		must(t, err)
		_, err = author.PatchContentVersion(t.Context(), own.ID, own.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Own edit", Body: "Only this post"}}})
		must(t, err)
		_, err = author.GetContent(t.Context(), existing.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = author.PatchContentVersion(t.Context(), existing.ID, existing.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Cannot edit owner", Body: "no"}}})
		requireError(t, err, 403, "forbidden")
		slug := "cannot-manage-channel"
		_, err = author.PatchChannel(t.Context(), string(channel.ID), channels.Update{Slug: &slug})
		requireError(t, err, 403, "forbidden")
		_, _, foreignChannel := ownedChannel(t, w, w.apps[1], "Other app community")
		_, err = author.PostContents(t.Context(), newPost("Foreign app channel", string(foreignChannel.ID)))
		requireError(t, err, 403, "forbidden")
		_, err = author.PostContentPublish(t.Context(), own.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, author, own.ID, own.Versions[0].ID)
		draft, err := author.PostContents(t.Context(), newPost("Channel grant revocation", string(channel.ID)))
		must(t, err)
		authtest.RevokeRole(t, w.tenant[0], scope, iam.UserSubject(alice.ID), w.channelContributor)
		_, err = author.GetContent(t.Context(), draft.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = author.PatchContentVersion(t.Context(), own.ID, own.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Cannot use app grant here", Body: "no"}}})
		requireError(t, err, 403, "forbidden")
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(bob.ID), w.channelWriter)
		_, err = credited.PatchContentVersion(t.Context(), existing.ID, existing.SelectedVersionID, catalog.VersionInput{Text: map[string]catalog.VersionText{"en": {Title: "Team editor still works", Body: "Team edit"}}})
		must(t, err)
		persona, err := w.auth.Persona("channel")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(bob.ID), persona.OwnerRole())
		authtest.RevokeRole(t, w.tenant[0], scope, iam.UserSubject(channelOwner.ID), persona.OwnerRole())
		_, err = owner.GetContent(t.Context(), existing.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = owner.PostContents(t.Context(), newPost("Owner metadata cannot bypass revocation", string(channel.ID)))
		requireError(t, err, 403, "forbidden")
		mine, err = owner.GetMeChannels(t.Context(), ck.GetMeChannelsQuery{})
		must(t, err)
		if len(mine.Channels) != 0 {
			t.Fatalf("revoked owner metadata still grants channel capabilities: %+v", mine.Channels)
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
		cfg.Content.Channels.Creation = "staff"
		cfg.Content.Channels.MaxPerOwner = 0
	})
	a := w.apps[0]
	attachMedia(t, w, a)
	channel, err := a.engine.Client().PostChannels(t.Context(), channels.Input{Slug: "official", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Official community"}}})
	must(t, err)
	if channel.OwnerUserID != "" || channel.AuthScope == "" {
		t.Fatal("site channel fixture has wrong ownership")
	}
	id, err := ckauth.ChannelScopeID(*a.cfg.Scope, channel.AuthScope)
	must(t, err)
	group := iam.GroupByID(id)
	manager, token := w.person(t, 0)
	client := a.client(token)
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelManager)
	_, err = client.PatchChannel(t.Context(), string(channel.ID), channels.Update{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Renamed by manager"}}})
	must(t, err)
	_, err = client.PostChannels(t.Context(), channels.Input{Slug: "not-authorized"})
	requireError(t, err, 403, "forbidden")
	payload := imageBytes(t)
	ref := media.RefBody{Kind: "channel", ID: string(channel.ID)}
	presign := media.PresignBody{Ref: ref, Path: "cover/new.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)}
	_, err = client.PostMediaUploadPresign(t.Context(), presign)
	must(t, err)
	authtest.RevokeRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelManager)
	_, err = client.PostMediaUploadPresign(t.Context(), presign)
	requireError(t, err, 403, "forbidden")
	_, err = client.PatchChannel(t.Context(), string(channel.ID), channels.Update{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Revoked manager"}}})
	requireError(t, err, 403, "forbidden")
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(manager.ID), w.channelWriter)
	input := catalog.ContentInput{Kind: "article", ChannelID: string(channel.ID), Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Official post", Body: "Public"}}}}}
	item, err := client.PostContents(t.Context(), input)
	must(t, err)
	_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, client, item.ID, item.Versions[0].ID)
	_, commentToken := w.person(t, 0)
	comment := postComment(t, a.client(commentToken), item.ID, "Moderated site channel")
	moderator, moderatorToken := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], group, iam.UserSubject(moderator.ID), w.channelModerator)
	must(t, a.client(moderatorToken).PostCommentHold(t.Context(), comment.ID))
	authtest.RevokeRole(t, w.tenant[0], group, iam.UserSubject(moderator.ID), w.channelModerator)
	err = a.client(moderatorToken).PostCommentHold(t.Context(), comment.ID)
	requireError(t, err, 403, "forbidden")
}

// A lost provider acknowledgement leaves a local pending channel. Its caller
// retries the same import identity; neither a queue nor a later sweep is needed.
func TestPendingChannelScopeHasNoAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	provider := &lostChannelScopeAck{Scopes: a.deps.Scopes, scope: *a.cfg.Scope}
	provider.lose.Store(true)
	a.deps.Scopes = provider
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	owner := authtest.NewUser(t, a.tenant)
	owner.TOTP = authtest.EnrollTOTP(t, a.tenant, owner)
	token := authtest.SignIn(t, a.tenant, owner).AccessToken
	caller := a.client(token)
	id := contentref.NewID()
	batch := importer.Batch{Source: "pending-channel-scope", ProvisionScopes: true, Channels: []importer.ChannelRecord{{ExternalID: "pending", ID: id, Slug: "pending-scope", OwnerUserID: owner.ID}}}
	result, err := a.engine.Client().PostImport(t.Context(), batch)
	if err == nil && len(result.Failed) == 0 {
		t.Fatal("lost provider acknowledgement was reported as success")
	}
	current, err := a.engine.Client().GetManageChannel(t.Context(), id, ck.GetManageChannelQuery{})
	must(t, err)
	if current.AuthScope != "" {
		t.Fatal("pending channel advertised authorization readiness")
	}
	input := catalog.ContentInput{Kind: "article", ChannelID: id, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Retryable channel post", Body: "One post"}}}}}
	_, err = caller.PostContents(t.Context(), input)
	requireError(t, err, 403, "forbidden")
	_, err = caller.PatchChannel(t.Context(), id, channels.Update{})
	requireError(t, err, 403, "forbidden")
	payload := imageBytes(t)
	presign := media.PresignBody{Ref: media.RefBody{Kind: "channel", ID: id}, Path: "cover/probe.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)}
	_, err = caller.PostMediaUploadPresign(t.Context(), presign)
	requireError(t, err, 403, "forbidden")
	mine, err := caller.GetMeChannels(t.Context(), ck.GetMeChannelsQuery{})
	must(t, err)
	if len(mine.Channels) != 0 {
		t.Fatalf("pending scope advertised authoring rights: %+v", mine.Channels)
	}
	result, err = a.engine.Client().PostImport(t.Context(), batch)
	must(t, err)
	if len(result.Failed) != 0 {
		t.Fatalf("retry failed: %+v", result.Failed)
	}
	current, err = a.engine.Client().GetManageChannel(t.Context(), id, ck.GetManageChannelQuery{})
	must(t, err)
	if current.ID != id || current.AuthScope != id {
		t.Fatalf("retry changed identity or did not finish: %+v", current)
	}
	_, err = caller.PostContents(t.Context(), input)
	must(t, err)
	_, err = caller.PostMediaUploadPresign(t.Context(), presign)
	must(t, err)
	var jobs int
	must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+" WHERE kind IN ('contentkit_entity_sync','contentkit_channel_lifecycle','contentkit_group_sync') AND args->>'tenant'=$1", a.cfg.Scope.AppID.String()).Scan(&jobs))
	if jobs != 0 {
		t.Fatalf("synchronous lifecycle queued %d jobs", jobs)
	}
}

type lostChannelScopeAck struct {
	entities.Scopes
	scope ck.Scope
	lose  atomic.Bool
}

func (s *lostChannelScopeAck) ContentKitScope() (string, string) {
	return s.scope.TenantID.String(), s.scope.AppID.String()
}
func (s *lostChannelScopeAck) Provision(ctx context.Context, groups []entities.Scope) error {
	if err := s.Scopes.Provision(ctx, groups); err != nil {
		return err
	}
	if s.lose.Swap(false) {
		return errors.New("injected lost channel provisioning acknowledgement")
	}
	return nil
}
func (s *lostChannelScopeAck) HoldingScopes(ctx context.Context, user string, kinds, ids []string) (map[string][]string, error) {
	return s.Scopes.(entities.LocalScopes).HoldingScopes(ctx, user, kinds, ids)
}
