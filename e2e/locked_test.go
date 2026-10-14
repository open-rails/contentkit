package e2e

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
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
	_, staff := w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	bill := premiumBilling(w, premium)
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
				c.Media.Config.Kinds[i].Uploads = append(k.Uploads, media.Upload{Path: "covers/{name}", Named: true, Max: 1, Types: []string{"image/png"}, MaxBytes: 1 << 20})
			}
		}
		d.Billing = bill
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
		f.want(http.StatusOK, staff, "PATCH", "/contents/"+c.ID+"/versions/"+c.Versions[0].ID, catalog.VersionInput{Cover: &cover}, nil)
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
			if c.Title != kind+" title" || c.Cover == nil || c.Cover.Image != cover || ver.Cover == nil || ver.Cover.Image != cover || len(c.Groups) != 1 ||
				c.Decision.Allowed != v.opens ||
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

// A locked gallery's raw media read over the real engine, AuthKit and
// OpenRails lists none of its pages: a viewer without access gets the
// version's public cover and that it holds locked files, never a page's path, size,
// count or the item's meta. A premium holder and staff get every page.
func TestLockedMediaListsNoPages(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, staff := w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	bill := premiumBilling(w, premium)
	images := []string{"image/png"}
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		gallery := c.Content.Kinds["gallery"]
		gallery.Locked = &kinds.Locked{Show: []string{kinds.ShowCover, kinds.ShowGroups}}
		c.Content.Kinds["gallery"] = gallery
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "gallery", KeepOriginals: true, ServeOriginals: true,
			Uploads: []media.Upload{{Path: "cover", Types: images, MaxBytes: 1 << 20}, {Path: "pages/{name}", Types: images, MaxBytes: 1 << 20}},
			Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{200}}}})
		d.Billing = bill
	}))

	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "gallery", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}},
		Text: map[string]catalog.Text{"en": {Title: "Premium pages"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
	version := media.RefBody{Kind: "gallery", ID: c.ID, Version: c.Versions[0].ID}
	ops := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "Premium pages"}}}
	for i := range 3 {
		plan := upload(t, f, staff, version, fmt.Sprintf("pages/%03d.png", i+1), pngBytes(t, 16+i))
		ops = append(ops, media.Op{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob})
	}
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: version, OperationID: uuid.NewString(), Ops: ops}, nil)
	versionRef, err := w.reg.VersionRef("gallery", c.ID, version.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.jobs.Manifests().Place(ctx, versionRef); err != nil {
		t.Fatal(err)
	}
	coverPlan := upload(t, f, staff, version, "cover", pngBytes(t, 32))
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: version, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: coverPlan.Path, Blob: coverPlan.Blob}}}, nil)
	cover := publish(t, w, versionRef, coverPlan.Path, "cover")

	pages := "/media/gallery/" + c.ID + "/versions/" + version.Version
	for _, v := range []struct{ name, token string }{{"anonymous", ""}, {"stranger", stranger}} {
		var read media.ReadResult
		rec := f.want(http.StatusOK, v.token, "GET", pages+"?limit=1", nil, &read)
		if read.Access != media.AccessNone || !read.Locked || read.Total != 0 || len(read.Files) != 0 || read.Meta != nil || strings.Contains(rec.Body.String(), "pages/") {
			t.Fatalf("%s reads the locked pages: %s", v.name, rec.Body)
		}
		if len(read.Public) != 1 || !reflect.DeepEqual(read.Public[0], cover) {
			t.Fatalf("%s reads the cover: %+v", v.name, read.Public)
		}
		f.want(http.StatusOK, v.token, "GET", pages+"?editor", nil, &read)
		if read.Total != 0 || len(read.Files) != 0 {
			t.Fatalf("%s reads the locked pages as an editor: %+v", v.name, read)
		}
	}
	for _, v := range []struct{ name, token string }{{"premium", premiumToken}, {"staff", staff}} {
		var read media.ReadResult
		f.want(http.StatusOK, v.token, "GET", pages+"?prefix=pages/", nil, &read)
		if read.Access != media.AccessFull || read.Locked || read.Total != 3 || len(read.Files) != 3 || read.Meta["title"] != "Premium pages" {
			t.Fatalf("%s reads the pages: %+v", v.name, read)
		}
		for i, fi := range read.Files {
			if fi.Path != fmt.Sprintf("pages/%03d.png", i+1) || fi.URL == "" || fi.Size == 0 {
				t.Fatalf("%s reads page %d: %+v", v.name, i, fi)
			}
		}
	}
	var edit media.ReadResult
	f.want(http.StatusOK, staff, "GET", pages+"?editor&prefix=pages/", nil, &edit)
	if edit.Total != 3 || !edit.Files[0].Upload || len(edit.Uploads) == 0 {
		t.Fatalf("staff's editor read %+v", edit)
	}
}

// premiumBilling is the site's merchant on an embedded OpenRails, holder
// entitled to its premium tier.
func premiumBilling(w *world, holder authtest.User) access.Billing {
	w.t.Helper()
	ctx := w.t.Context()
	merchant := channelBilling(w)
	customer := billing.CustomerID(uuid.MustParse(holder.ID))
	if _, err := merchant.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{}); err != nil {
		w.t.Fatal(err)
	}
	product, err := merchant.CreateProduct(ctx, billing.CreateProductParams{Key: "premium", DisplayName: "Premium", Entitlements: []string{"premium"}})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := merchant.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{
		Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}}); err != nil {
		w.t.Fatal(err)
	}
	return ckopenrails.Billing(merchant)
}
