package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/kindsroute"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// What a viewer without access sees of premium content is its kind's locked
// view, over the real engine, AuthKit and OpenRails: books show Doujins' view
// (cover and groups), galleries every property and two preview pages. A
// premium holder sees all of both; anyone else gets of a book its title,
// cover and groups and none of its description, attributes, segments, counts
// or comments, and may neither react nor comment, while an album shows them
// everything but its body and takes their reactions and comments. Favoriting
// needs only visibility.
func TestLockedContentFollowsItsKindsView(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, staff := w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	merchant := channelBilling(w)
	customer := billing.CustomerID(uuid.MustParse(premium.ID))
	if _, err := merchant.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{}); err != nil {
		t.Fatal(err)
	}
	product, err := merchant.CreateProduct(ctx, billing.CreateProductParams{Key: "premium", DisplayName: "Premium", Entitlements: []string{"premium"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := merchant.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{
		Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}}); err != nil {
		t.Fatal(err)
	}
	format := map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"manga"}}}
	e := w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		book := c.Content.Kinds["book"]
		book.Groups, book.Attributes = []string{"tag"}, format
		book.Locked = &kinds.Locked{Show: []string{kinds.ShowCover, kinds.ShowGroups}}
		c.Content.Kinds["book"] = book
		album := c.Content.Kinds["album"]
		album.Groups, album.Attributes = []string{"tag"}, format
		album.Locked = &kinds.Locked{Show: kinds.Shown, Preview: &kinds.Preview{Pages: 2}}
		c.Content.Kinds["album"] = album
		for i, k := range c.Media.Config.Kinds {
			if k.Name == "book" {
				c.Media.Config.Kinds[i].Uploads = append(k.Uploads, media.Upload{Path: "covers/{name}", Content: true, Named: true, Max: 1, Types: []string{"image/png"}, MaxBytes: 1 << 20})
			}
		}
		d.Billing = ckopenrails.Billing(merchant)
	})
	f := serve(t, e)

	var tags []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/groups", []groups.GroupInput{{Kind: "tag", Slug: "vanilla",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Vanilla"}}}}, &tags)
	premiumRule := access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	cover := "i-" + uuid.NewString()
	seed := func(kind string) string {
		t.Helper()
		v := catalog.VersionInput{Languages: []string{"en"}}
		if kind == "book" {
			v.Segments = &[]catalog.Segment{{Start: 0, End: 10, Title: "One"}}
		}
		var c catalog.Content
		f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: kind, Access: &premiumRule, Attributes: map[string]any{"format": "manga"},
			Text: map[string]catalog.Text{"en": {Title: kind + " title", Description: kind + " teaser", Body: "the paid part"}}, Versions: []catalog.VersionInput{v}}, &c)
		f.want(http.StatusOK, staff, "PATCH", "/contents/"+c.ID, catalog.ContentPatch{Cover: &cover}, nil)
		f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/groups", groups.ContentGroups{Memberships: []groups.Membership{{GroupID: tags[0].ID}}}, nil)
		f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
		f.want(http.StatusOK, premiumToken, "POST", "/contents/"+c.ID+"/like", nil, nil)
		f.want(http.StatusOK, premiumToken, "POST", "/contents/"+c.ID+"/favorite", nil, nil)
		f.want(http.StatusCreated, premiumToken, "POST", "/contents/"+c.ID+"/comments", content.CommentInput{Body: kind + " is great"}, nil)
		return c.ID
	}
	book, album := seed("book"), seed("album")

	var model kindsroute.ContentModel
	f.want(http.StatusOK, "", "GET", "/kinds", nil, &model)
	if l := model.Kinds["book"].Locked; l == nil || !slices.Equal(l.Show, []string{kinds.ShowCover, kinds.ShowGroups}) || l.Preview != nil {
		t.Fatalf("the model's book view %+v", l)
	}
	if l := model.Kinds["gallery"].Locked; l == nil || !slices.Equal(l.Show, kinds.DefaultLocked.Show) {
		t.Fatalf("the model's default view %+v", l)
	}

	type viewer struct {
		name, token string
		opens       bool
	}
	viewers := []viewer{{"premium", premiumToken, true}, {"anonymous", "", false}, {"stranger", stranger, false}}
	for _, kind := range []string{"book", "album"} {
		id := map[string]string{"book": book, "album": album}[kind]
		for _, v := range viewers {
			shows := v.opens || kind == "album"
			name := kind + " to " + v.name

			var c catalog.Content
			f.want(http.StatusOK, v.token, "GET", "/contents/"+id+"?language=en", nil, &c)
			ver := c.Versions[0]
			if c.Title != kind+" title" || c.Cover == nil || c.Cover.Image != cover || len(c.Groups) != 1 || c.Decision.Allowed != v.opens ||
				(c.Body != "") != v.opens || (c.Description != "") != shows || (len(c.Attributes) > 0) != shows || (c.Views != nil) != shows {
				t.Fatalf("%s: detail %+v", name, c)
			}
			if kind == "book" && (len(ver.Segments) > 0) != shows {
				t.Fatalf("%s: segments %+v", name, ver.Segments)
			}
			if p := c.Decision.Preview; !v.opens && kind == "album" && (p == nil || p.Pages != 2) || (v.opens || kind == "book") && p != nil {
				t.Fatalf("%s: preview %+v", name, p)
			}

			var page discovery.ContentPage
			f.want(http.StatusOK, v.token, "GET", "/contents?kind="+kind, nil, &page)
			card := page.Contents[0]
			n := card.Counts
			if len(page.Contents) != 1 || card.Title != kind+" title" || len(card.Groups["tag"]) != 1 || (len(card.Attributes) > 0) != shows ||
				(n.Likes != nil && n.Dislikes != nil) != shows || (n.Favorites != nil) != shows || (n.Views != nil) != shows {
				t.Fatalf("%s: card %+v", name, card)
			}

			var rc content.ReactionCounts
			f.want(http.StatusOK, v.token, "GET", "/contents/"+id+"/reaction", nil, &rc)
			if (rc.Likes != nil && *rc.Likes == 1) != shows {
				t.Fatalf("%s: reactions %+v", name, rc)
			}
			var fav content.FavoriteState
			f.want(http.StatusOK, v.token, "GET", "/contents/"+id+"/favorite", nil, &fav)
			if (fav.Count != nil && *fav.Count == 1) != shows {
				t.Fatalf("%s: favorites %+v", name, fav)
			}

			var thread []content.Comment
			f.want(http.StatusOK, v.token, "GET", "/contents/"+id+"/comments", nil, &thread)
			var standing content.CommentStanding
			f.want(http.StatusOK, v.token, "GET", "/contents/"+id+"/can-comment", nil, &standing)
			if (len(thread) == 1) != shows || standing.Locked == shows {
				t.Fatalf("%s: thread %+v standing %+v", name, thread, standing)
			}
			latest := f.want(http.StatusOK, v.token, "GET", "/comments/latest?limit=100", nil, nil).Body.String()
			if strings.Contains(latest, kind+" is great") != shows {
				t.Fatalf("%s: latest feed %s", name, latest)
			}
		}
	}

	// A stranger reacts and comments where the view shows reactions and
	// comments, and favorites either.
	for _, x := range []struct {
		id     string
		status int
		code   string
	}{{book, http.StatusForbidden, "forbidden"}, {album, http.StatusOK, ""}} {
		rec := f.call(stranger, "POST", "/contents/"+x.id+"/dislike", nil)
		if rec.Code != x.status || !strings.Contains(rec.Body.String(), x.code) {
			t.Fatalf("a stranger's dislike: %d %s", rec.Code, rec.Body)
		}
		status, code := http.StatusCreated, ""
		if x.id == book {
			status, code = http.StatusForbidden, content.CodeCommentsLocked
		}
		if rec := f.call(stranger, "POST", "/contents/"+x.id+"/comments", content.CommentInput{Body: "me too"}); rec.Code != status || !strings.Contains(rec.Body.String(), code) {
			t.Fatalf("a stranger's comment: %d %s", rec.Code, rec.Body)
		}
		var fav content.FavoriteState
		f.want(http.StatusOK, stranger, "POST", "/contents/"+x.id+"/favorite", nil, &fav)
		if !fav.Favorited || (fav.Count != nil) != (x.id == album) {
			t.Fatalf("a stranger's favorite %+v", fav)
		}
	}
	var rc content.ReactionCounts
	f.want(http.StatusOK, premiumToken, "GET", "/contents/"+book+"/reaction", nil, &rc)
	if *rc.Likes != 1 || *rc.Dislikes != 0 || rc.Mine != 1 {
		t.Fatalf("the book's reactions %+v", rc)
	}
}
