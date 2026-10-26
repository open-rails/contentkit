package critical_test

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
)

// Reposting references one original through the public API. It never copies
// versions or turns the receiving team into editors of the original.
func TestFirstClassChannelRepostAuthority(t *testing.T) {
	// AuthKit and ContentKit deliberately share one connection: permission
	// lookups must complete before repost transactions hold that connection.
	dsn := os.Getenv("CONTENTKIT_CRITICAL_PG")
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		must(t, err)
		query := parsed.Query()
		query.Set("pool_max_conns", "1")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else if dsn != "" {
		dsn += " pool_max_conns=1"
	}
	t.Setenv("CONTENTKIT_CRITICAL_PG", dsn)
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.Channels.Entitled = true
		kind := cfg.Content.Kinds["article"]
		kind.Versions.Hidden = false
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	startPublicationRuntime(t, a)
	_, sourceToken, source := ownedChannel(t, w, a, "Original author")
	destinationOwner, destinationToken, destination := ownedChannel(t, w, a, "Receiving channel")
	originalClient, destinationClient := a.client(sourceToken), a.client(destinationToken)
	create := func(title string) catalog.Content {
		item, err := originalClient.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: source.ID,
			Text:     map[string]catalog.Text{"en": {Title: title, Body: "One original body"}},
			Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"de"}}}})
		must(t, err)
		return item
	}
	original := create("Original with two languages")
	_, err := originalClient.PostContentPublish(t.Context(), original.ID, catalog.PublishInput{})
	must(t, err)
	for _, version := range original.Versions {
		awaitEditionPublication(t, a, originalClient, original.ID, version.ID)
	}
	t.Run("reference_preserves_identity_without_editorial_authority", func(t *testing.T) {
		first, err := destinationClient.PutChannelRepost(t.Context(), destination.ID, original.ID)
		must(t, err)
		second, err := destinationClient.PutChannelRepost(t.Context(), destination.ID, original.ID)
		must(t, err)
		if first.ChannelID != destination.ID || first.ActorID != destinationOwner.ID || !first.CreatedAt.Equal(second.CreatedAt) {
			t.Fatalf("idempotent repost identity changed: first=%+v second=%+v", first, second)
		}
		feed, err := destinationClient.GetChannelContents(t.Context(), destination.ID, ck.GetChannelContentsQuery{})
		must(t, err)
		if len(feed.Contents) != 1 {
			t.Fatalf("duplicate or missing original: %+v", feed.Contents)
		}
		card := feed.Contents[0]
		if card.ID != original.ID || card.Channel == nil || card.Channel.ID != source.ID || card.Repost == nil || card.Repost.ChannelID != destination.ID || !card.Repost.CanRemove {
			t.Fatalf("repost replaced original ownership or lost removal authority: %+v", card)
		}
		_, err = destinationClient.PutContentText(t.Context(), original.ID, "en", catalog.Text{Title: "Cannot edit another channel", Body: "bad"})
		requireError(t, err, 403, "forbidden")
		_, err = destinationClient.PatchContentVersion(t.Context(), original.ID, original.Versions[1].ID, catalog.VersionInput{Text: map[string]catalog.VersionText{"de": {Title: "Cannot edit translated version"}}})
		requireError(t, err, 403, "forbidden")
		_, err = destinationClient.PostContentVersions(t.Context(), original.ID, catalog.VersionInput{Languages: []string{"en"}})
		requireError(t, err, 403, "forbidden")
		must(t, destinationClient.DeleteChannelRepost(t.Context(), destination.ID, original.ID))
		feed, err = destinationClient.GetChannelContents(t.Context(), destination.ID, ck.GetChannelContentsQuery{})
		must(t, err)
		if len(feed.Contents) != 0 {
			t.Fatal("removed reference remains in destination feed")
		}
		still, err := a.client("").GetContent(t.Context(), original.ID, ck.GetContentQuery{})
		must(t, err)
		if still.ID != original.ID || len(still.Versions) != 2 {
			t.Fatal("removing repost modified original versions")
		}
	})
	t.Run("pending_source_channel_is_not_readable_or_repostable", func(t *testing.T) {
		_, err := destinationClient.PutChannelRepost(t.Context(), destination.ID, original.ID)
		must(t, err)
		table := pgx.Identifier{w.schema, "content_entities"}.Sanitize()
		_, err = w.pool.Exec(t.Context(), "UPDATE "+table+" SET auth_scope=NULL,revision=revision+1 WHERE tenant_id=$1 AND id=$2", a.cfg.Scope.AppID.String(), source.ID)
		must(t, err)
		restore := func() {
			_, err := w.pool.Exec(t.Context(), "UPDATE "+table+" SET auth_scope=id,revision=revision+1 WHERE tenant_id=$1 AND id=$2", a.cfg.Scope.AppID.String(), source.ID)
			must(t, err)
		}
		defer restore()
		_, err = a.client("").GetContent(t.Context(), original.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = a.client("").GetVersion(t.Context(), original.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		for _, reader := range []*ck.Client{a.client(""), destinationClient, a.engine.Client()} {
			feed, err := reader.GetChannelContents(t.Context(), destination.ID, ck.GetChannelContentsQuery{})
			must(t, err)
			if len(feed.Contents) != 0 {
				t.Fatalf("pending original leaked in destination feed: %+v", feed.Contents)
			}
		}
		_, err = a.engine.Client().PutChannelRepost(t.Context(), destination.ID, original.ID)
		requireError(t, err, 404, "not_found")
	})
	t.Run("drafts_and_withdrawn_originals_never_appear_in_public_feeds", func(t *testing.T) {
		draft := create("Private draft")
		_, err := a.engine.Client().PutChannelRepost(t.Context(), destination.ID, draft.ID)
		requireError(t, err, 404, "not_found")
		_, err = destinationClient.PutChannelRepost(t.Context(), destination.ID, original.ID)
		must(t, err)
		_, err = originalClient.PostContentUnpublish(t.Context(), original.ID)
		must(t, err)
		for _, reader := range []*ck.Client{a.client(""), destinationClient, a.engine.Client()} {
			feed, err := reader.GetChannelContents(t.Context(), destination.ID, ck.GetChannelContentsQuery{})
			must(t, err)
			if len(feed.Contents) != 0 {
				t.Fatalf("withdrawn original leaked through repost: %+v", feed.Contents)
			}
		}
	})
	t.Run("channel_authority_is_current_and_metadata_routes_cannot_retarget_it", func(t *testing.T) {
		_, err := destinationClient.PatchEntity(t.Context(), source.ID, entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: "name", Name: "Cannot change source channel as entity"}}})
		requireError(t, err, 404, "not_found")
		_, err = a.engine.Client().PutContentEntitiesShared(t.Context(), original.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: entities.EntityID(destination.ID)}}}, ck.PutContentEntitiesSharedQuery{})
		if err == nil {
			t.Fatal("channel accepted as descriptive metadata")
		}
		groupID, err := ckauth.ChannelScopeID(*a.cfg.Scope, destination.AuthScope)
		must(t, err)
		replacement, _ := w.person(t, 0)
		replacement.TOTP = authtest.EnrollTOTP(t, w.tenant[0], replacement)
		persona, err := w.auth.Persona("channel")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], iam.GroupByID(groupID), iam.UserSubject(replacement.ID), persona.OwnerRole())
		authtest.RevokeRole(t, w.tenant[0], iam.GroupByID(groupID), iam.UserSubject(destinationOwner.ID), persona.OwnerRole())
		_, err = destinationClient.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: destination.ID})
		requireError(t, err, 403, "forbidden")
		_, err = destinationClient.PatchChannel(t.Context(), destination.ID, channels.Update{})
		requireError(t, err, 403, "forbidden")
	})
	t.Run("all_versions_share_parent_channel_read_policy", func(t *testing.T) {
		parentRule := access.Rule{Level: access.ChannelLevel, ChannelID: source.ID}
		valid, err := originalClient.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: source.ID, Access: &parentRule,
			Text: map[string]catalog.Text{"en": {Title: "One parent policy"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"de"}}}})
		must(t, err)
		if len(valid.Versions) != 2 {
			t.Fatal("valid channel policy did not create both editions")
		}
		for _, version := range valid.Versions {
			if version.Access == nil || version.Access.ChannelID != source.ID || version.Access.Level != access.ChannelLevel {
				t.Fatal("edition did not inherit the valid parent rule")
			}
		}
		_, err = originalClient.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: source.ID, Access: &parentRule,
			Versions: []catalog.VersionInput{{Languages: []string{"en"}, Access: &access.Rule{Level: access.Public}}}})
		requireError(t, err, 400, "invalid_request")
		wrongChannel := access.Rule{Level: access.ChannelLevel, ChannelID: destination.ID}
		_, err = originalClient.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: source.ID, Access: &wrongChannel})
		requireError(t, err, 400, "invalid_request")
	})
}
