package critical_test

import (
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// Channel interaction routes use the channel identity, never a metadata entity
// or one of its posts. The existing generic interaction implementation must
// preserve that distinction through its new route family.
func TestChannelInteractionBindings(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", MembershipScopes: []kinds.MembershipScope{kinds.ContentScope}}
		cfg.Content.Channels.Entities = []string{"tag"}
	})
	a := w.apps[0]
	_, ownerToken, channel := ownedChannel(t, w, a, "Interaction channel")
	owner := a.client(ownerToken)
	public, err := a.client("").GetChannel(t.Context(), channel.ID, ck.GetChannelQuery{})
	must(t, err)
	if public.ID != channel.ID || public.Slug != channel.Slug {
		t.Fatal("public channel identity changed")
	}
	listed, err := a.client("").GetChannels(t.Context(), ck.GetChannelsQuery{Slug: channel.Slug})
	must(t, err)
	if len(listed.Channels) != 1 || listed.Channels[0].ID != channel.ID {
		t.Fatal("public channel directory lost exact slug match")
	}
	mine, err := owner.GetMeChannels(t.Context(), ck.GetMeChannelsQuery{})
	must(t, err)
	if len(mine.Channels) != 1 || mine.Channels[0].ID != channel.ID || !mine.Channels[0].Owner {
		t.Fatal("current owner missing from own channel list")
	}
	managed, err := a.engine.Client().GetManageChannels(t.Context(), ck.GetManageChannelsQuery{Slug: channel.Slug})
	must(t, err)
	if len(managed.Channels) != 1 || managed.Channels[0].ID != channel.ID {
		t.Fatal("management channel listing lost identity")
	}
	detailed, err := a.engine.Client().GetManageChannel(t.Context(), channel.ID, ck.GetManageChannelQuery{})
	must(t, err)
	if detailed.AuthScope != channel.AuthScope || detailed.OwnerUserID != channel.OwnerUserID {
		t.Fatal("management scope/owner identity changed")
	}
	tags, err := a.engine.Client().PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "channel-description", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Channel description"}}}})
	must(t, err)
	must(t, owner.PutChannelEntities(t.Context(), channel.ID, []string{string(tags[0].ID)}))
	metadata, err := owner.GetChannelEntities(t.Context(), channel.ID, ck.GetChannelEntitiesQuery{})
	must(t, err)
	if len(metadata) != 1 || metadata[0].ID != tags[0].ID {
		t.Fatal("channel descriptive metadata was not attached")
	}
	must(t, owner.PutChannelEntities(t.Context(), channel.ID, []string{}))
	metadata, err = owner.GetChannelEntities(t.Context(), channel.ID, ck.GetChannelEntitiesQuery{})
	must(t, err)
	if len(metadata) != 0 {
		t.Fatal("metadata replacement did not clear old associations")
	}
	_, token := w.person(t, 0)
	client := a.client(token)
	like, err := client.PostChannelLike(t.Context(), channel.ID)
	must(t, err)
	if like.Mine != 1 || like.Likes == nil || *like.Likes != 1 {
		t.Fatalf("like did not persist: %+v", like)
	}
	dislike, err := client.PostChannelDislike(t.Context(), channel.ID)
	must(t, err)
	if dislike.Mine != -1 || dislike.Likes == nil || *dislike.Likes != 0 || dislike.Dislikes == nil || *dislike.Dislikes != 1 {
		t.Fatalf("reaction switch did not replace old value: %+v", dislike)
	}
	read, err := client.GetChannelReaction(t.Context(), channel.ID)
	must(t, err)
	if read.Mine != -1 || read.Dislikes == nil || *read.Dislikes != 1 {
		t.Fatal("channel reaction read lost saved state")
	}
	cleared, err := client.DeleteChannelReaction(t.Context(), channel.ID)
	must(t, err)
	if cleared.Mine != 0 || cleared.Dislikes == nil || *cleared.Dislikes != 0 {
		t.Fatal("reaction delete did not clear state")
	}
	favorite, err := client.PostChannelFavorite(t.Context(), channel.ID)
	must(t, err)
	if !favorite.Favorited || favorite.Count == nil || *favorite.Count != 1 {
		t.Fatalf("favorite did not persist: %+v", favorite)
	}
	current, err := client.GetChannelFavorite(t.Context(), channel.ID)
	must(t, err)
	if !current.Favorited || current.Count == nil || *current.Count != 1 {
		t.Fatal("channel favorite read lost state")
	}
	removed, err := client.DeleteChannelFavorite(t.Context(), channel.ID)
	must(t, err)
	if removed.Favorited || removed.Count == nil || *removed.Count != 0 {
		t.Fatal("favorite delete did not clear state")
	}
	comment, err := client.PostChannelComments(t.Context(), channel.ID, content.CommentInput{Body: "A channel comment", Language: "en"})
	must(t, err)
	comments, err := client.GetChannelComments(t.Context(), channel.ID, ck.GetChannelCommentsQuery{Language: "en"})
	must(t, err)
	if len(comments) != 1 || comments[0].ID != comment.ID || comments[0].Body != "A channel comment" {
		t.Fatalf("channel comment identity changed: %+v", comments)
	}
	standing, err := client.GetChannelCanComment(t.Context(), channel.ID, ck.GetChannelCanCommentQuery{Language: "en"})
	must(t, err)
	if !standing.CanComment || standing.Count == nil || *standing.Count != 1 {
		t.Fatalf("channel standing/count wrong: %+v", standing)
	}
	_, err = client.GetEntityReaction(t.Context(), channel.ID)
	requireError(t, err, 404, "not_found")
}
