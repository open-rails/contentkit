package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
)

// Every file lives in a version, and one rule picks the edition a language
// opens: the first live version by position serving it, else the first live
// one. An album with English A, English B and Chinese Z, each with its own
// cover, shows A's on /en cards and opens A without ?v=; reordered, B's; on
// /zh, Z's. Detail, group covers and the sitemap follow the same rule, and a
// card a filter narrows to another version links to it with ?v=.
func TestEditionsCarryTheirOwnCovers(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(func(c *contentkit.Config, _ *contentkit.Deps) {
		c.Content.Languages = append(c.Content.Languages, "zh")
		album := c.Content.Kinds["album"]
		album.Groups = []string{"tag"}
		c.Content.Kinds["album"] = album
	}))

	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "album",
		Text: map[string]catalog.Text{"en": {Title: "Editions"}, "zh": {Title: "版本"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"en"}}, {Languages: []string{"zh"}}}}, &c)
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, &c)
	a, b, z := c.Versions[0], c.Versions[1], c.Versions[2]
	cover := map[string]string{} // version id -> its cover's URL
	for i, v := range []catalog.Version{a, b, z} {
		ref := media.RefBody{Kind: "album", ID: c.ID, Version: v.ID}
		plan := upload(t, f, staff, ref, "cover", pngBytes(t, 16+i))
		f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
		item, err := w.reg.VersionRef("album", c.ID, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		cover[v.ID] = publish(t, w, item, plan.Path, "cover").Renditions[0].URL
		if !strings.Contains(cover[v.ID], "/album/"+c.ID+"/"+v.ID+"/public/") {
			t.Fatalf("version %s cover at %s", v.ID, cover[v.ID])
		}
	}

	card := func(query string) discovery.Card {
		t.Helper()
		var page discovery.ContentPage
		f.want(http.StatusOK, "", "GET", "/contents?kind=album&"+query, nil, &page)
		if len(page.Contents) != 1 {
			t.Fatalf("%s: %d cards", query, len(page.Contents))
		}
		return page.Contents[0]
	}
	detail := func(language string) catalog.Content {
		t.Helper()
		var d catalog.Content
		f.want(http.StatusOK, "", "GET", "/contents/"+c.ID+"?language="+language, nil, &d)
		return d
	}
	// edition checks every surface shows v as language's edition.
	edition := func(language string, v catalog.Version) {
		t.Helper()
		got := card("display_language=" + language)
		if got.Version.ID != v.ID || len(got.Cover) == 0 || got.Cover[0].URL != cover[v.ID] || strings.Contains(got.Path, "?v=") ||
			!strings.HasPrefix(got.Path, "/"+language+"/") {
			t.Fatalf("%s card %+v, want version %s", language, got, v.ID)
		}
		d := detail(language)
		if def := catalog.DefaultVersion(d.Versions, language); def == nil || def.ID != v.ID || d.Cover == nil || d.Cover.URL == nil || *d.Cover.URL != cover[v.ID] {
			t.Fatalf("%s detail opens %+v with cover %+v, want %s", language, def, d.Cover, v.ID)
		}
		for _, dv := range d.Versions {
			if dv.Cover == nil || dv.Cover.URL == nil || *dv.Cover.URL != cover[dv.ID] {
				t.Fatalf("%s detail: version %s cover %+v", language, dv.ID, dv.Cover)
			}
		}
	}
	edition("en", a)
	edition("zh", z)
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/versions/order", catalog.OrderInput{IDs: []string{b.ID, a.ID, z.ID}}, nil)
	edition("en", b)
	edition("zh", z)
	if got := card("display_language=ja"); got.Version.ID != b.ID || got.Cover[0].URL != cover[b.ID] {
		t.Fatalf("a language no version serves opens the first: %+v", got)
	}

	// A version-level tag on A alone: its list's card is A, linked with ?v=.
	var tags []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/groups", []groups.GroupInput{{Kind: "tag", Slug: "first-edition",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "First edition"}}}}, &tags)
	tag := tags[0].ID
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/groups", groups.ContentGroups{Memberships: []groups.Membership{{GroupID: tag, VersionID: a.ID}}}, nil)
	if got := card("display_language=en&groups=" + string(tag)); got.Version.ID != a.ID || got.Cover[0].URL != cover[a.ID] ||
		!strings.HasSuffix(got.Path, "?v="+a.Code) {
		t.Fatalf("filtered card %+v", got)
	}

	// The tag's automatic cover is its member's edition in the reader's language.
	store, err := groups.New(groups.Options{Pool: w.pool, Schema: w.schema, Tenant: w.tenant, Kinds: w.config().Content})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Recount(t.Context()); err != nil {
		t.Fatal(err)
	}
	for language, v := range map[string]catalog.Version{"en": b, "zh": z} {
		var g groups.GroupDetail
		f.want(http.StatusOK, "", "GET", "/groups/"+string(tag)+"?language="+language, nil, &g)
		if g.Cover == nil || !g.Cover.Auto || g.Cover.ContentID != c.ID || len(g.Cover.Renditions) == 0 || g.Cover.Renditions[0].URL != cover[v.ID] {
			t.Fatalf("%s group cover %+v, want %s's", language, g.Cover, v.ID)
		}
	}

	// The sitemap's page in each language shows that language's edition.
	var sm discovery.SitemapPage
	f.want(http.StatusOK, "", "GET", "/sitemap/contents?kind=album", nil, &sm)
	i := slices.IndexFunc(sm.Entries, func(e discovery.SitemapEntry) bool { return e.ID == c.ID })
	if i < 0 || len(sm.Entries[i].Languages) != 2 {
		t.Fatalf("sitemap %+v", sm)
	}
	for _, l := range sm.Entries[i].Languages {
		want := map[string]string{"en": cover[b.ID], "zh": cover[z.ID]}[l.Language]
		if l.Image == "" || l.Image != want {
			t.Fatalf("sitemap %s image %q, want %q", l.Language, l.Image, want)
		}
	}
}

// A body's images are each version's own copies: one taken into a version
// is copied into its siblings, and a new version starts with them. A locked
// version's copies reach only those who open it.
func TestBodyImagesLiveInEachVersion(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	bill := premiumBilling(w, premium)
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		for i, k := range c.Media.Config.Kinds {
			if k.Name == "album" {
				k.Uploads = append(k.Uploads, media.Upload{Path: "inline/{name}", Body: true, Named: true, Max: 10, Types: []string{"image/png"}, MaxBytes: 1 << 20})
				k.Public = append(k.Public, media.Public{Name: "inline", From: "inline/{name}", To: "inline-{name}-{w}.webp", Widths: []int{64}})
				c.Media.Config.Kinds[i] = k
			}
		}
		d.Billing = bill
	}))
	body := media.BodySuffix
	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "album", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}},
		Text:     map[string]catalog.Text{"en": {Title: "Illustrated", Body: "soon"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}}}}, &c)
	a, b := c.Versions[0], c.Versions[1]
	ref := media.RefBody{Kind: "album" + body, ID: c.ID, Version: a.ID}
	plan := upload(t, f, staff, ref, "inline/photo.png", pngBytes(t, 20))
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	item, err := w.reg.VersionRef("album"+body, c.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.jobs.Manifests().Place(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(plan.Path, "inline/"), ".png")
	// The worker's private rendition for openers, stood in for over the upload's blob.
	if _, err := w.jobs.Manifests().EditExisting(t.Context(), item, func(m *media.Manifest) error {
		i := m.Find(plan.Path)
		m.Files = append(m.Files, media.File{Path: "inline-open/" + name + ".webp", Blob: m.Files[i].Blob, Type: "image/webp", Size: 1,
			From: plan.Path, Preset: "inline-open"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var inline catalog.InlineImage
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/images", catalog.BodyImageInput{Image: &name, Version: a.ID}, &inline)
	f.want(http.StatusBadRequest, staff, "POST", "/contents/"+c.ID+"/images", catalog.BodyImageInput{Image: &name, Version: uuid.Must(uuid.NewV7()).String()}, nil)
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/text/en", catalog.Text{Title: "Illustrated", Body: "see " + inline.Ref}, nil)
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)

	holds := func(version string) bool {
		ref, err := w.reg.VersionRef("album"+body, c.ID, version)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := w.jobs.Manifests().Get(t.Context(), ref)
		return err == nil && m.Find(plan.Path) >= 0
	}
	eventually(t, "the sibling's copy", func() bool { return holds(b.ID) })
	var added catalog.Version
	f.want(http.StatusCreated, staff, "POST", "/contents/"+c.ID+"/versions", catalog.VersionInput{Languages: []string{"en"}, State: catalog.StatePublished}, &added)
	eventually(t, "the new version's copy", func() bool { return holds(added.ID) })

	// Locked: the body and every version's copies stay with those who open it.
	for _, who := range []string{"", stranger} {
		var d catalog.Content
		f.want(http.StatusOK, who, "GET", "/contents/"+c.ID+"?language=en", nil, &d)
		if d.Body != "" || len(d.Images) != 0 {
			t.Fatalf("a locked viewer is shown the body: %+v", d)
		}
		for _, v := range []string{a.ID, b.ID, added.ID} {
			f.want(http.StatusNotFound, who, "GET", "/media/album"+body+"/"+c.ID+"/versions/"+v, nil, nil)
		}
	}
	var opened catalog.Content
	f.want(http.StatusOK, premiumToken, "GET", "/contents/"+c.ID+"?language=en", nil, &opened)
	u, ok := opened.Images[name]
	if !ok || !strings.Contains(opened.Body, u) || !strings.Contains(u, "/album"+body+"/"+c.ID+"/"+a.ID+"/private/") {
		t.Fatalf("the opener's body %+v", opened)
	}
}

// eventually waits up to a minute for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !ok(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
