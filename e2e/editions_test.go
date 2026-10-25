package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/token"
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
		album.Entities = []string{"tag"}
		c.Content.Kinds["album"] = album
	}))

	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "album",
		Text:     map[string]catalog.Text{"en": {Title: "Editions"}, "zh": {Title: "版本"}},
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
		if !strings.HasPrefix(cover[v.ID], "https://media.test/v1/"+w.tenant+"/album/"+v.ID+"/public/") {
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
	var tags []entities.Entity
	f.want(http.StatusCreated, staff, "POST", "/entities", []entities.EntityInput{{Kind: "tag", Slug: "first-edition",
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "First edition"}}}}, &tags)
	tag := tags[0].ID
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/entities", entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tag, VersionID: a.ID}}}, nil)
	if got := card("display_language=en&groups=" + string(tag)); got.Version.ID != a.ID || got.Cover[0].URL != cover[a.ID] ||
		!strings.HasSuffix(got.Path, "?v="+a.Code) {
		t.Fatalf("filtered card %+v", got)
	}

	// The tag's automatic cover is its member's edition in the reader's language.
	store, err := entities.New(entities.Options{Pool: w.pool, Schema: w.schema, Tenant: w.tenant, Kinds: w.config().Content})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Recount(t.Context()); err != nil {
		t.Fatal(err)
	}
	for language, v := range map[string]catalog.Version{"en": b, "zh": z} {
		var g entities.EntityDetail
		f.want(http.StatusOK, "", "GET", "/entities/"+string(tag)+"?language="+language, nil, &g)
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

// A body's images are each version's own private files: one taken into a
// version is copied into its siblings, and a new version starts with them.
// A locked version gets its readers no URL or token; its openers' URLs open
// through the gateway with the version's item token, which opens no other
// version's.
func TestBodyImagesLiveInEachVersion(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, staff := w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	bill := premiumBilling(w, premium)
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		for i, k := range c.Media.Config.Kinds {
			if k.Name == "album" {
				k.Uploads = append(k.Uploads, media.Upload{Path: "inline/{name}", Body: true, Named: true, Max: 10, Types: []string{"image/png"}, MaxBytes: 1 << 20})
				k.Private = append(k.Private, media.Private{Name: "inline", From: "inline/{name}", To: "shown/{name}.webp", Image: &media.Image{Width: 64}})
				c.Media.Config.Kinds[i] = k
			}
		}
		d.Billing = bill
	}))
	key, err := token.ParseKey(w.config().Media.Delivery.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := token.NewRing(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := w.env.Config
	gw, err := gateway.New(gateway.Config{Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
		Ring: ring, Hosts: map[string][]string{"media.test": {w.tenant}}, Buckets: map[string]string{w.tenant: cfg.Bucket}})
	if err != nil {
		t.Fatal(err)
	}
	media0 := httptest.NewServer(gw)
	t.Cleanup(media0.Close)
	fetch := func(u string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, media0.URL+strings.TrimPrefix(u, "https://media.test"), nil)
		req.Host = "media.test"
		resp, err := media0.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "album", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}},
		Text:     map[string]catalog.Text{"en": {Title: "Illustrated", Body: "soon"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}}}}, &c)
	a, b := c.Versions[0], c.Versions[1]
	ref := media.RefBody{Kind: "album", ID: c.ID, Version: a.ID}
	plan := upload(t, f, staff, ref, "inline/photo.png", pngBytes(t, 20))
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	itemOf := func(version string) contentref.ContentRef {
		r, err := w.reg.VersionRef("album", c.ID, version)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err := w.jobs.Manifests().Place(ctx, itemOf(a.ID)); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(plan.Path, "inline/"), ".png")
	var inline catalog.InlineImage
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/images", catalog.BodyImageInput{Image: &name, Version: a.ID}, &inline)
	f.want(http.StatusBadRequest, staff, "POST", "/contents/"+c.ID+"/images", catalog.BodyImageInput{Image: &name, Version: uuid.Must(uuid.NewV7()).String()}, nil)
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/text/en", catalog.Text{Title: "Illustrated", Body: "see " + inline.Ref}, nil)
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)

	// rendered is the version's body image rendered (the worker stood in for
	// over its own copy's blob): nil until the copy lands.
	rendered := func(version string) *media.File {
		m, _, err := w.jobs.Manifests().Get(ctx, itemOf(version))
		if err != nil {
			return nil
		}
		i := m.Find(plan.Path)
		if i < 0 || m.Files[i].Blob == "" {
			return nil
		}
		out := media.File{Path: "shown/" + name + ".webp", Blob: m.Files[i].Blob, Type: "image/webp", Size: 1, From: plan.Path, Preset: "inline"}
		if _, err := w.jobs.Manifests().EditExisting(ctx, itemOf(version), func(m *media.Manifest) error {
			if m.Find(out.Path) < 0 {
				m.Files = append(m.Files, out)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	var added catalog.Version
	f.want(http.StatusCreated, staff, "POST", "/contents/"+c.ID+"/versions", catalog.VersionInput{Languages: []string{"en"}, State: catalog.StatePublished}, &added)
	files := map[string]*media.File{}
	for _, v := range []string{a.ID, b.ID, added.ID} {
		eventually(t, "the body image in version "+v, func() bool { files[v] = rendered(v); return files[v] != nil })
	}

	// Locked: neither the body nor a token for its files.
	for _, who := range []string{"", stranger} {
		var d catalog.Content
		f.want(http.StatusOK, who, "GET", "/contents/"+c.ID+"?language=en", nil, &d)
		if d.Body != "" || len(d.Images) != 0 {
			t.Fatalf("a locked viewer is shown the body: %+v", d)
		}
		var read media.ReadResult
		rec := f.want(http.StatusOK, who, "GET", "/media/album/"+c.ID+"/versions/"+a.ID, nil, &read)
		if read.Access == media.AccessFull || len(read.Files) != 0 || strings.Contains(rec.Body.String(), "t=") || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("a locked viewer is handed the body's files: %s", rec.Body)
		}
	}
	// An opener's body shows the version's private file, which opens with
	// its item token.
	var opened catalog.Content
	f.want(http.StatusOK, premiumToken, "GET", "/contents/"+c.ID+"?language=en", nil, &opened)
	u, ok := opened.Images[name]
	if !ok || !strings.Contains(opened.Body, u) || !strings.HasPrefix(u, "https://media.test/v1/"+w.tenant+"/album/"+a.ID+"/private/") || fetch(u) != http.StatusOK {
		t.Fatalf("the opener's body %+v", opened)
	}
	if strings.Contains(u, "t=") && fetch(strings.Split(u, "?")[0]) == http.StatusOK {
		t.Fatal("a body image opens without its token")
	}

	// A free version opens to everyone, its siblings' files only to theirs.
	public := access.Rule{Level: access.Public}
	f.want(http.StatusOK, staff, "PATCH", "/contents/"+c.ID+"/versions/"+a.ID, catalog.VersionInput{Access: &public}, nil)
	var free catalog.Content
	f.want(http.StatusOK, stranger, "GET", "/contents/"+c.ID+"?language=en", nil, &free)
	u, ok = free.Images[name]
	if !ok || !strings.HasPrefix(u, "https://media.test/v1/"+w.tenant+"/album/"+a.ID+"/private/") || fetch(u) != http.StatusOK {
		t.Fatalf("the free version's body %+v", free)
	}
	item, _ := w.reg.Item(itemOf(b.ID))
	other, _ := item.Blob(files[b.ID].Blob)
	parsed, _ := url.Parse(u)
	parsed.Path = "/v1/" + other
	if code := fetch(parsed.String()); code == http.StatusOK {
		t.Fatalf("a token of version %s opened locked version %s's body image: %d", a.ID, b.ID, code)
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
