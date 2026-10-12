package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
)

// A channel's post carries its channel as owner on cards and on its page:
// name, path and the avatar's and cover's renditions, resolved by the media
// batch. The channel's feed lists only its posts.
func TestOwnerChip(t *testing.T) {
	w := newWorld(t)
	_, creatorToken := w.person(iam.Role{})
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.GroupKinds["channel"] = channel
		d.Scopes = ckauthkit.Scopes(w.ak)
	}))
	var made []groups.Group
	f.want(http.StatusCreated, creatorToken, "POST", "/groups", []groups.GroupInput{{Kind: "channel", Slug: "studio",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio"}}}}, &made)
	ch := made[0]
	ref := media.RefBody{Kind: "channel", ID: string(ch.ID)}
	itemRef, err := w.reg.Ref("channel", string(ch.ID))
	if err != nil {
		t.Fatal(err)
	}
	picture := func(path, preset string, size int) (string, media.PublicImage) {
		plan := upload(t, f, creatorToken, ref, path, pngBytes(t, size))
		f.want(http.StatusOK, creatorToken, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
		return strings.TrimSuffix(strings.TrimPrefix(plan.Path, preset+"/"), ".png"), publish(t, w, itemRef, plan.Path, preset)
	}
	avatar, avatarImage := picture("avatar/me.png", "avatar", 32)
	cover, coverImage := picture("cover/wide.png", "cover", 48)
	f.want(http.StatusOK, creatorToken, "PATCH", "/groups/"+string(ch.ID), groups.GroupUpdate{Avatar: &groups.Avatar{Image: avatar},
		Cover: &groups.Cover{Image: cover}}, nil)

	var owned catalog.Content
	f.want(http.StatusCreated, creatorToken, "POST", "/contents", catalog.ContentInput{Kind: "blog", OwnerGroupID: string(ch.ID),
		Text: map[string]catalog.Text{"en": {Title: "Hello"}}}, &owned)
	f.want(http.StatusOK, creatorToken, "POST", "/contents/"+owned.ID+"/publish", catalog.PublishInput{}, nil)
	w.published(f, staff, "blog")

	chip := func(name string, o *groups.GroupCard) {
		t.Helper()
		switch {
		case o == nil || o.ID != ch.ID || o.Kind != "channel" || o.Slug != "studio" || o.Name != "Studio" || o.Path == "":
			t.Fatalf("%s: owner %+v", name, o)
		case o.Avatar == nil || len(o.Avatar.Renditions) == 0 || o.Avatar.Renditions[0].URL != avatarImage.Renditions[0].URL:
			t.Fatalf("%s: avatar %+v, published %+v", name, o.Avatar, avatarImage)
		case o.Cover == nil || len(o.Cover.Renditions) == 0 || o.Cover.Renditions[0].URL != coverImage.Renditions[0].URL:
			t.Fatalf("%s: cover %+v, published %+v", name, o.Cover, coverImage)
		}
	}
	var page discovery.ContentPage
	f.want(http.StatusOK, "", "GET", "/contents?kind=blog&language=en&owner_group="+string(ch.ID), nil, &page)
	if len(page.Contents) != 1 || page.Contents[0].ID != owned.ID {
		t.Fatalf("the channel's feed %+v", page.Contents)
	}
	chip("card", page.Contents[0].Owner)
	f.want(http.StatusOK, "", "GET", "/contents?kind=blog&language=en", nil, &page)
	if len(page.Contents) != 2 {
		t.Fatalf("every post %+v", page.Contents)
	}
	var detail catalog.Content
	f.want(http.StatusOK, "", "GET", "/contents/"+owned.ID+"?language=en", nil, &detail)
	chip("detail", detail.Owner)
}
