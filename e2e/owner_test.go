package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/offers"
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

// A channel's editors (channel:content:write, not the author) open its
// members-only posts and are offered nothing of it: editor and the body on
// the page, every version of its gallery open without a sale, open cards
// without offers, its drafts in its feed, the channel's page with no
// membership offer, and the channel among their groups as its writer. A
// non-member who edits nothing stays locked out and is offered it all.
// Members-only posts need the channel to sell a membership first.
func TestOwnerGroupEditors(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, creatorToken := w.person(iam.Role{})
	writer, writerToken := w.person(iam.Role{})
	_, strangerToken := w.person(iam.Role{})
	merchant := channelBilling(w)
	e := w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.GroupKinds["channel"] = channel
		d.Billing, d.Scopes = ckopenrails.Billing(merchant), ckauthkit.Scopes(w.ak)
	})
	f := serve(t, e)
	var made []groups.Group
	f.want(http.StatusCreated, creatorToken, "POST", "/groups", []groups.GroupInput{{Kind: "channel", Slug: "studio",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio"}}}}, &made)
	ch := string(made[0].ID)
	authtest.GrantRole(t, w.ak, iam.GroupByID(made[0].AuthScope), iam.UserSubject(writer.ID), w.channelWriter)
	membersOnly := catalog.ContentInput{Kind: "blog", OwnerGroupID: ch, Access: &access.Rule{Level: access.GroupLevel, Group: ch},
		Text: map[string]catalog.Text{"en": {Title: "Backstage", Description: "Teaser", Body: "Members words"}}}
	var refusal struct{ Code string }
	f.want(http.StatusConflict, creatorToken, "POST", "/contents", membersOnly, &refusal)
	if refusal.Code != "no_membership" {
		t.Fatalf("members-only on a channel selling nothing answered %q", refusal.Code)
	}
	monthly := 720
	f.want(http.StatusOK, creatorToken, "PUT", "/groups/"+ch+"/offer", offers.Terms{Amount: 4_990_000, Currency: "USD", RenewHours: &monthly}, nil)

	var members, draft, gallery catalog.Content
	f.want(http.StatusCreated, creatorToken, "POST", "/contents", membersOnly, &members)
	f.want(http.StatusOK, creatorToken, "POST", "/contents/"+members.ID+"/publish", catalog.PublishInput{}, nil)
	f.want(http.StatusCreated, creatorToken, "POST", "/contents", catalog.ContentInput{Kind: "blog", OwnerGroupID: ch,
		Text: map[string]catalog.Text{"en": {Title: "Next week"}}}, &draft)
	// A gallery whose first version is free and second sold on its own.
	f.want(http.StatusCreated, creatorToken, "POST", "/contents", catalog.ContentInput{Kind: "gallery", OwnerGroupID: ch,
		Text:     map[string]catalog.Text{"en": {Title: "Sketches"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}, Access: &access.Rule{Level: access.PPV}}}}, &gallery)
	f.want(http.StatusOK, creatorToken, "POST", "/contents/"+gallery.ID+"/publish", catalog.PublishInput{}, nil)
	f.want(http.StatusOK, creatorToken, "PUT", "/contents/"+gallery.ID+"/versions/"+gallery.Versions[1].ID+"/offer",
		offers.Terms{Amount: 1_990_000, Currency: "USD"}, nil)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		list, err := e.Client().GetOffers(ctx, contentkit.GetOffersQuery{Unsynced: ptr(true)})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Offers) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("offers never synced: %+v", list.Offers)
		}
	}

	var d catalog.Content
	f.want(http.StatusOK, writerToken, "GET", "/contents/"+members.ID+"?language=en", nil, &d)
	if !d.Editor || !d.Decision.Allowed || len(d.Decision.Sell) > 0 || len(d.Decision.Offers) > 0 || d.Body != "Members words" {
		t.Fatalf("the channel's writer reads editor %v, decision %+v, body %q", d.Editor, d.Decision, d.Body)
	}
	d = catalog.Content{}
	f.want(http.StatusOK, strangerToken, "GET", "/contents/"+members.ID+"?language=en", nil, &d)
	if d.Editor || d.Decision.Allowed || d.Body != "" || len(d.Decision.Offers) != 1 || d.Decision.Offers[0].PriceID == "" {
		t.Fatalf("a stranger reads editor %v, decision %+v, body %q", d.Editor, d.Decision, d.Body)
	}
	f.want(http.StatusOK, writerToken, "GET", "/contents/"+gallery.ID, nil, &d)
	for _, v := range d.Versions {
		if !v.Decision.Allowed || len(v.Decision.Sell) > 0 || len(v.Decision.Offers) > 0 {
			t.Fatalf("the writer is offered the channel's gallery: version %s %+v", v.ID, v.Decision)
		}
	}
	f.want(http.StatusOK, strangerToken, "GET", "/contents/"+gallery.ID, nil, &d)
	if len(d.Versions) != 2 || !d.Versions[0].Decision.Allowed || d.Versions[1].Decision.Allowed || len(d.Versions[1].Decision.Offers) != 1 {
		t.Fatalf("a stranger's gallery %+v", d.Versions)
	}

	cards := func(token, path string) map[string]discovery.CardAccess {
		t.Helper()
		var page discovery.ContentPage
		f.want(http.StatusOK, token, "GET", path, nil, &page)
		out := map[string]discovery.CardAccess{}
		for _, c := range page.Contents {
			out[c.ID] = c.Access
		}
		return out
	}
	feed := "/contents?kind=blog&language=en&owner_group=" + ch
	if got := cards(writerToken, feed); len(got) != 2 || !got[members.ID].Allowed || len(got[members.ID].Offers) > 0 || !got[draft.ID].Allowed {
		t.Fatalf("the writer's feed %+v", got)
	}
	if got := cards(strangerToken, feed); len(got) != 1 || got[members.ID].Allowed || len(got[members.ID].Offers) != 1 {
		t.Fatalf("a stranger's feed %+v", got)
	}
	if got := cards(writerToken, "/contents?kind=blog&language=en"); len(got) != 1 || !got[members.ID].Allowed {
		t.Fatalf("the writer's list %+v", got)
	}

	var page groups.GroupDetail
	for token, editor := range map[string]bool{creatorToken: true, writerToken: true, strangerToken: false} {
		f.want(http.StatusOK, token, "GET", "/groups/"+ch, nil, &page)
		if a := page.Access; a == nil || a.Editor != editor || (a.Offer == nil) != editor {
			t.Fatalf("the channel's page as an editor (%v): access %+v", editor, a)
		}
	}
	var mine groups.MyGroupPage
	f.want(http.StatusOK, writerToken, "GET", "/me/groups", nil, &mine)
	if len(mine.Groups) != 1 || string(mine.Groups[0].ID) != ch || !mine.Groups[0].Writer || mine.Groups[0].Editor || mine.Groups[0].Owner {
		t.Fatalf("the writer's groups %+v", mine)
	}
}
