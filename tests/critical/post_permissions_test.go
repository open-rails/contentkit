package critical_test

import (
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
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
		suggestions, err := credited.GetManageSearchSuggestions(t.Context(), ck.GetManageSearchSuggestionsQuery{Q: "Personal contributor", ContentKinds: []string{"article"}})
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
	t.Run("CK-J03-S01_channel_contributor_cannot_edit_other_posts_or_channel", func(t *testing.T) {
		_, ownerToken, channel := ownedEntity(t, w, a, "Community")
		owner := a.client(ownerToken)
		existing, err := owner.PostContents(t.Context(), newPost("Owner private draft", string(channel.ID)))
		must(t, err)
		_, err = author.PostContents(t.Context(), newPost("App grant is not channel grant", string(channel.ID)))
		requireError(t, err, 403, "forbidden")
		scopeID, err := ckauth.EntityScopeID(*a.cfg.Scope, channel.AuthScope)
		must(t, err)
		scope := iam.GroupByID(scopeID)
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(alice.ID), w.channelContributor)
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
		authtest.GrantRole(t, w.tenant[0], scope, iam.UserSubject(bob.ID), w.channelWriter)
		_, err = credited.PutContentText(t.Context(), existing.ID, "en", catalog.Text{Title: "Team editor still works", Body: "Team edit"})
		must(t, err)
	})
}
