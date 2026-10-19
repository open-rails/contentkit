package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

// downloads is a site selling premium with movies (downloads serve the
// original) and books (pages, marked where the book's watermark says) and
// its media host: the real gateway over the bucket.
type downloads struct {
	w       *world
	f       *server
	gateway *httptest.Server
	ring    token.Ring
	staff   string
	premium string // holds premium through OpenRails
	user    string
	another string
	// translators are doujins-com, embargo and anonymous (categorical).
	translators []groups.Group
}

// join puts c's version in the translator groups of slugs.
func (d *downloads) join(c catalog.Content, slugs ...string) {
	d.w.t.Helper()
	var ms []groups.Membership
	for _, slug := range slugs {
		i := slices.IndexFunc(d.translators, func(g groups.Group) bool { return g.Slug == slug })
		ms = append(ms, groups.Membership{GroupID: d.translators[i].ID, VersionID: c.Versions[0].ID})
	}
	d.f.want(http.StatusOK, d.staff, "PUT", "/contents/"+c.ID+"/entities", groups.ContentGroups{Memberships: ms}, nil)
}

// policy is a host's download policy, as Doujins' legacy rules go: a
// version embargoed (translator:embargo) waits three days after its release
// for viewers without premium; a premium version translated by the site
// (translator:doujins-com) has pages 3, 5 and 7 marked. It names marks for
// files too, which ContentKit ignores. A translated version's download is
// named "[Translators] Title (language)", leaving out categorical ones.
type policy struct{}

func (policy) Decide(_ context.Context, r versions.DownloadRequest) (versions.DownloadDecision, error) {
	if r.In(kinds.GroupRef{Kind: "translator", Slug: "embargo"}) && !slices.Contains(r.Tiers, "premium") && !r.Editor {
		if at := r.Released.Add(72 * time.Hour); time.Now().Before(at) {
			return versions.DownloadDecision{Reason: "release_window", AvailableAt: &at}, nil
		}
	}
	d := versions.DownloadDecision{Allowed: true}
	if r.In(kinds.GroupRef{Kind: "translator", Slug: "doujins-com"}) && r.Rule.Level == access.TierLevel && slices.Contains(r.Rule.Tiers, "premium") {
		d.Mark = []int{3, 5, 7}
	}
	if r.Pages == 0 {
		d.Mark = []int{1, 2}
	}
	var by []string
	for _, g := range r.Groups {
		if g.Kind == "translator" && g.Attributes["categorical"] != true {
			by = append(by, g.Name)
		}
	}
	if len(by) > 0 {
		d.Name = "[" + strings.Join(by, ", ") + "] " + r.Title + " (" + r.Language + ")"
	}
	return d, nil
}

// Mark appends who and which page.
func (policy) Mark(_ context.Context, a access.Actor, _ versions.Target, page int, b []byte) ([]byte, string, error) {
	return append(bytes.Clone(b), fmt.Sprintf("|marked %s p%d", a.ID, page)...), ".webp", nil
}

func newDownloads(t *testing.T, quotas versions.Downloads) *downloads {
	w := newWorld(t)
	ctx := t.Context()
	d := &downloads{w: w}
	_, d.staff = w.person(w.staff)
	premium, premiumToken := w.person(iam.Role{})
	_, d.user = w.person(iam.Role{})
	_, d.another = w.person(iam.Role{})
	d.premium = premiumToken
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
	e := w.engine(func(c *contentkit.Config, deps *contentkit.Deps) {
		c.Content.Kinds["movie"] = kinds.Kind{Route: "movies", Presentation: "player", Versions: kinds.VersionRules{Languages: "one"},
			Groups: []string{"translator"}}
		c.Content.GroupKinds["translator"] = kinds.GroupKind{Route: "translators", VersionLevel: true,
			Attributes: map[string]kinds.Attribute{"categorical": {Type: "bool"}}}
		book := c.Content.Kinds["book"]
		book.Groups = []string{"translator"}
		c.Content.Kinds["book"] = book
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "movie", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4", "video/x-matroska"}, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{}}}})
		c.Media.Downloads = quotas
		deps.Billing, deps.Downloads = ckopenrails.Billing(merchant), policy{}
	})
	d.f = serve(t, e)
	d.f.want(http.StatusCreated, d.staff, "POST", "/entities", []groups.GroupInput{
		{Kind: "translator", Slug: "doujins-com", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Doujins.com"},
			{Language: "ja", Kind: groups.NameCanonical, Name: "ドウジンズ"}}},
		{Kind: "translator", Slug: "embargo", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Embargo"}}},
		{Kind: "translator", Slug: "anonymous", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Anonymous"}},
			Attributes: map[string]any{"categorical": true}}}, &d.translators)
	key, err := token.ParseKey(w.config().Media.Delivery.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	if d.ring, err = token.NewRing(key, nil); err != nil {
		t.Fatal(err)
	}
	cfg := w.env.Config
	gw, err := gateway.New(gateway.Config{Endpoint: cfg.Endpoint, Region: cfg.Region, AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
		Ring: d.ring, Hosts: map[string][]string{"media.test": {w.tenant}}, Buckets: map[string]string{w.tenant: cfg.Bucket}})
	if err != nil {
		t.Fatal(err)
	}
	d.gateway = httptest.NewServer(gw)
	t.Cleanup(d.gateway.Close)
	return d
}

// fetch GETs a media URL from the gateway.
func (d *downloads) fetch(u string) (int, []byte, http.Header) {
	d.w.t.Helper()
	req, _ := http.NewRequestWithContext(d.w.t.Context(), http.MethodGet, d.gateway.URL+strings.TrimPrefix(u, "https://media.test"), nil)
	req.Host = "media.test"
	resp, err := d.gateway.Client().Do(req)
	if err != nil {
		d.w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// content creates and publishes a content of kind under rule, titled title,
// and returns it.
func (d *downloads) content(kind, title string, rule access.Rule) catalog.Content {
	return d.contentIn(kind, rule, "en", map[string]catalog.Text{"en": {Title: title}})
}

// contentIn is content with a version in language, titled by text.
func (d *downloads) contentIn(kind string, rule access.Rule, language string, text map[string]catalog.Text) catalog.Content {
	d.w.t.Helper()
	var c catalog.Content
	d.f.want(http.StatusCreated, d.staff, "POST", "/contents", catalog.ContentInput{Kind: kind, Access: &rule,
		Text: text, Versions: []catalog.VersionInput{{Languages: []string{language}}}}, &c)
	d.f.want(http.StatusOK, d.staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, &c)
	return c
}

// put uploads body at path of a version through presign, the bucket and
// commit, as the admin does, and places it as the worker does first.
func (d *downloads) put(c catalog.Content, path, typ string, body []byte) contentref.ContentRef {
	d.w.t.Helper()
	ref := media.RefBody{Kind: c.Kind, ID: c.ID, Version: c.Versions[0].ID}
	sum := sha256.Sum256(body)
	var plan media.PresignReply
	d.f.want(http.StatusOK, d.staff, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: path, Type: typ, Size: int64(len(body)),
		SHA256: hex.EncodeToString(sum[:])}, &plan)
	req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.w.t.Fatal(err)
	}
	resp.Body.Close()
	d.f.want(http.StatusOK, d.staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	r, err := d.w.reg.VersionRef(c.Kind, c.ID, c.Versions[0].ID)
	if err != nil {
		d.w.t.Fatal(err)
	}
	if _, err := d.w.jobs.Manifests().Place(d.w.t.Context(), r); err != nil {
		d.w.t.Fatal(err)
	}
	return r
}

// download asks for c's download as bearer and wants status.
func (d *downloads) download(status int, bearer string, c catalog.Content) *httptest.ResponseRecorder {
	d.w.t.Helper()
	return d.f.want(status, bearer, "GET", "/contents/"+c.ID+"/versions/"+c.Versions[0].ID+"/download", nil, nil)
}

func (d *downloads) file(bearer string, c catalog.Content) versions.FileDownload {
	d.w.t.Helper()
	var out versions.FileDownload
	if err := json.Unmarshal(d.download(http.StatusOK, bearer, c).Body.Bytes(), &out); err != nil {
		d.w.t.Fatal(err)
	}
	return out
}

func (d *downloads) refused(bearer string, c catalog.Content) time.Duration {
	d.w.t.Helper()
	var reply struct {
		Code       string `json:"code"`
		RetryAfter int    `json:"retry_after"`
	}
	if err := json.Unmarshal(d.download(http.StatusTooManyRequests, bearer, c).Body.Bytes(), &reply); err != nil || reply.Code != "rate_limited" || reply.RetryAfter <= 0 {
		d.w.t.Fatalf("quota refusal %+v %v", reply, err)
	}
	return time.Duration(reply.RetryAfter) * time.Second
}

// A movie's download is its original upload at a short-lived URL of the
// media host, never through the app: the gateway serves exactly its bytes as
// an attachment named for the movie and its version, to that URL alone and
// only briefly; the viewing token never opens it; the policy's marks don't
// apply. Locked movies download only for entitled viewers and editors, and
// the host's policy may hold one back (403 download_unavailable with when),
// charging nothing. Every download is charged to its viewer's bucket:
// anonymous by address, signed in, or the premium tier, which replaces the
// user bucket.
func TestMovieDownloadsServeTheOriginal(t *testing.T) {
	d := newDownloads(t, versions.Downloads{
		Anonymous: []versions.Quota{{Limit: 2, Per: time.Hour}},
		User:      []versions.Quota{{Limit: 3, Per: time.Hour}},
		Tiers:     []versions.TierQuota{{Tier: "premium", Quotas: []versions.Quota{{Limit: 5, Per: time.Hour}}}},
		TTL:       10 * time.Second})
	free := d.content("movie", "Night Train: Episode 2", access.Rule{Level: access.Public})
	premium := d.content("movie", "Locked Movie", access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}})
	empty := d.content("movie", "Nothing Yet", access.Rule{Level: access.Public})
	embargoed := d.content("movie", "Just Out", access.Rule{Level: access.Public})
	d.join(embargoed, "embargo")
	d.put(embargoed, "source", "video/mp4", []byte("embargoed original"))
	original := bytes.Repeat([]byte("original video bytes "), 500)
	ref := d.put(free, "source", "video/x-matroska", original)
	d.put(premium, "source", "video/mp4", []byte("premium original"))

	// The original sits in download/.
	m, _, err := d.w.jobs.Manifests().Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := m.Get("source.mkv")
	if area, _, _ := layout.SplitBlobRef(src.Blob); area != layout.AreaDownload {
		t.Fatalf("the original is in %s/: %+v", area, src)
	}

	// The policy names marks for every file; a video ignores them.
	got := d.file("", free)
	name := "Night Train: Episode 2 [" + free.Versions[0].Code + "].mkv"
	if got.Name != name || got.Type != "video/x-matroska" || got.Size != int64(len(original)) || time.Until(got.ExpiresAt) > 10*time.Second {
		t.Fatalf("file download %+v", got)
	}
	status, body, hdr := d.fetch(got.URL)
	if status != http.StatusOK || !bytes.Equal(body, original) || hdr.Get("Content-Disposition") != layout.Attachment(name) {
		t.Fatalf("the media host served %d %q (%d bytes)", status, hdr.Get("Content-Disposition"), len(body))
	}
	// The URL opens its file alone, and only until it expires.
	item, _ := d.w.reg.Item(ref)
	other, _ := item.Blob(layout.BlobRef(layout.AreaDownload, layout.BlobName(make([]byte, 32), uuid.NewString())))
	if status, _, _ := d.fetch(strings.Replace(got.URL, "/"+strings.Split(src.Blob, "/")[1], "/"+path.Base(other), 1)); status != http.StatusNotFound {
		t.Fatalf("the download token opened another key: %d", status)
	}
	// The viewing (item) token never opens the original.
	scope := token.ItemScope(ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version())
	key, _ := item.Blob(src.Blob)
	viewing := "https://media.test/v1/" + key + "?t=" + d.ring.Sign(scope, time.Now().Add(time.Hour))
	if status, _, _ := d.fetch(viewing); status != http.StatusNotFound {
		t.Fatalf("the item token opened the original: %d", status)
	}
	time.Sleep(time.Until(got.ExpiresAt) + time.Second)
	if status, _, _ := d.fetch(got.URL); status != http.StatusNotFound {
		t.Fatalf("an expired download URL: %d", status)
	}

	// Anonymous: two per address, the second charged again.
	d.file("", free)
	d.refused("", free)
	// The policy holds the embargoed movie back from viewers without premium
	// for three days after its release, charging nothing.
	for range 2 {
		var refusal struct {
			Code        string    `json:"code"`
			Reason      string    `json:"reason"`
			AvailableAt time.Time `json:"available_at"`
		}
		if err := json.Unmarshal(d.download(http.StatusForbidden, d.user, embargoed).Body.Bytes(), &refusal); err != nil ||
			refusal.Code != "download_unavailable" || refusal.Reason != "release_window" ||
			time.Until(refusal.AvailableAt) < 71*time.Hour || time.Until(refusal.AvailableAt) > 72*time.Hour {
			t.Fatalf("embargo refusal %+v %v", refusal, err)
		}
	}
	// Signed in: three, apart from the anonymous bucket; refusals took none.
	for range 3 {
		d.file(d.user, free)
	}
	d.refused(d.user, free)
	d.file(d.another, free)
	// Premium: its tier's five replace the user's three; the embargo
	// does not hold it back.
	for range 3 {
		d.file(d.premium, free)
	}
	// The policy names a translated movie's download, from the facts it is given.
	f := d.file(d.premium, embargoed)
	if f.Size != int64(len("embargoed original")) || f.Name != "[Embargo] Just Out (en).mp4" {
		t.Fatalf("premium embargoed download %+v", f)
	}
	if status, _, hdr := d.fetch(f.URL); status != http.StatusOK || hdr.Get("Content-Disposition") != layout.Attachment(f.Name) {
		t.Fatalf("the media host served the embargoed movie %d %q", status, hdr.Get("Content-Disposition"))
	}
	if f := d.file(d.premium, premium); f.Type != "video/mp4" || !strings.HasSuffix(f.Name, "].mp4") {
		t.Fatalf("premium download %+v", f)
	}
	d.refused(d.premium, premium)

	// Locked: refused unless entitled; staff always pass.
	d.download(http.StatusForbidden, "", premium)
	d.download(http.StatusForbidden, d.another, premium)
	if f := d.file(d.staff, premium); f.URL == "" {
		t.Fatalf("staff download %+v", f)
	}
	// A quota for a tier the content model lacks is refused at New.
	cfg := d.w.config()
	cfg.Media.Downloads = versions.Downloads{Tiers: []versions.TierQuota{{Tier: "gold", Quotas: []versions.Quota{{Limit: 1, Per: time.Hour}}}}}
	if e, err := contentkit.New(t.Context(), cfg, d.w.deps()); err == nil || !strings.Contains(err.Error(), `"gold"`) {
		if e != nil {
			_ = e.Close(context.Background())
		}
		t.Fatalf("a quota of an unknown tier: %v", err)
	}
	// Nothing uploaded: a clear 409.
	var reply struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(d.download(http.StatusConflict, d.staff, empty).Body.Bytes(), &reply); err != nil || reply.Code != versions.CodeNoDownload {
		t.Fatalf("no file: %+v %v", reply, err)
	}
}

// pages uploads n pages to c's version and stands in for the image worker:
// each page's web rendition in private/, its full-resolution copy in
// download/ and the zip of those. It returns the zip's bytes.
func (d *downloads) pages(c catalog.Content, n int) []byte {
	t := d.w.t
	t.Helper()
	ctx := t.Context()
	var ref contentref.ContentRef
	for i := range n {
		ref = d.put(c, fmt.Sprintf("pages/%03d", i+1), "image/png", pngBytes(t, 8+i))
	}
	ms := d.w.jobs.Manifests()
	item, _ := d.w.reg.Item(ref)
	k := item.Kind()
	m, _, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	copyTo := func(src media.File, area string) string {
		sum, _ := layout.BlobDigest(src.Blob)
		refs, err := ms.NewBlobs(ctx, ref, area, [][]byte{sum})
		if err != nil {
			t.Fatal(err)
		}
		from, _ := item.Blob(src.Blob)
		to, _ := item.Blob(refs[0])
		if _, err := d.w.env.Store.Copy(ctx, from, to, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		return refs[0]
	}
	type outs struct {
		from       string
		read, high media.File
	}
	var produced []outs
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for _, f := range m.Files {
		if !f.IsUpload() || !strings.HasPrefix(f.Path, "pages/") {
			continue
		}
		stem := strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))
		o := outs{from: f.Path,
			read: media.File{Path: "read/" + stem + ".webp", Blob: copyTo(f, k.Area("", "read")), Type: "image/webp", Size: f.Size, FP: "e2e"},
			high: media.File{Path: "high/" + stem + ".webp", Blob: copyTo(f, k.Area("", "high")), Type: "image/webp", Size: f.Size, FP: "e2e"}}
		produced = append(produced, o)
		e, _ := zw.CreateHeader(&zip.FileHeader{Name: path.Base(o.high.Path), Method: zip.Store})
		key, _ := item.Blob(f.Blob)
		rc, _, err := d.w.env.Store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(e, rc)
		rc.Close()
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		for _, o := range produced {
			if err := m.SetOutputs(o.from, "read", []media.File{o.read}); err != nil {
				return err
			}
			if err := m.SetOutputs(o.from, "high", []media.File{o.high}); err != nil {
				return err
			}
			m.ClearPending(o.from, "read")
			m.ClearPending(o.from, "high")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipped.Bytes())
	refs, err := ms.NewBlobs(ctx, ref, k.Area("", "zip"), [][]byte{sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := item.Blob(refs[0])
	if _, err := d.w.env.Store.Put(ctx, key, bytes.NewReader(zipped.Bytes()), int64(zipped.Len()), media.PutOptions{ContentType: "application/zip"}); err != nil {
		t.Fatal(err)
	}
	// The zip packs its inputs as the stored manifest orders them.
	if _, err := ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		z := &k.Private[slices.IndexFunc(k.Private, func(p media.Private) bool { return p.Name == "zip" })]
		return m.SetOutputs(z.Zip, z.Name, []media.File{{Path: z.To, Blob: refs[0], Type: "application/zip", Size: int64(zipped.Len()),
			FP: media.ZipFP(k.ZipInputs(m, z))}})
	}); err != nil {
		t.Fatal(err)
	}
	// This helper stands in for the producer. Freeze the same source order a
	// real worker publishes; download authorization must not mix draft pages
	// with another publication's chapter positions.
	m, _, err = ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var pages []map[string]any
	for _, f := range m.Files {
		if f.IsUpload() && strings.HasPrefix(f.Path, "pages/") {
			pages = append(pages, map[string]any{"path": f.Path, "source": f.Key()})
		}
	}
	snapshot, _ := json.Marshal(map[string]any{"pages": pages, "spans": []any{}})
	if _, err := d.w.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{d.w.schema, "content_version_publications"}.Sanitize()+`
	(tenant_id,id,version_id,kind,state,source) VALUES ($1,$2,$3,'pages','current',$4)`, d.w.tenant, contentref.NewID(), c.Versions[0].ID, snapshot); err != nil {
		t.Fatal(err)
	}
	return zipped.Bytes()
}

func entries(t *testing.T, b []byte) [][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, f := range zr.File {
		rc, _ := f.Open()
		e, _ := io.ReadAll(rc)
		rc.Close()
		out = append(out, e)
	}
	return out
}

// A book's download is its full-resolution pages as a zip. The host's policy
// marks exactly pages 3, 5 and 7 of a premium version the site translated,
// streamed through the archive route; every other page, and every page of a
// premium version outside the group or a free one in it, is byte-identical
// to the configured download rendition. The policy names a
// translated version's zip from its title and groups' names in its language,
// leaving out a group by its attributes; the archive route serves only the
// name its ticket was granted. The
// originals and full-resolution pages never open to the viewing token; the
// web rendition does.
func TestBookDownloadsMarkThePolicysPages(t *testing.T) {
	d := newDownloads(t, versions.Downloads{})
	premium := access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	ours := d.contentIn("book", premium, "ja", map[string]catalog.Text{"en": {Title: "Ours"}, "ja": {Title: "私たちの本"}})
	d.join(ours, "doujins-com")
	theirs := d.content("book", "Theirs", premium)
	free := d.content("book", "Free", access.Rule{Level: access.Public})
	d.join(free, "doujins-com", "anonymous")
	prepared := map[string][]byte{}
	for _, c := range []catalog.Content{ours, theirs, free} {
		prepared[c.ID] = d.pages(c, 8)
	}

	// Ours (in Japanese), to a premium viewer: the archive route, marking as
	// it streams, named in Japanese.
	granted := d.file(d.premium, ours)
	const oursName = "[ドウジンズ] 私たちの本 (ja).zip"
	if !strings.HasPrefix(granted.URL, "archive?") || granted.Name != oursName || granted.Type != "application/zip" {
		t.Fatalf("marked download %+v", granted)
	}
	archive := "/contents/" + ours.ID + "/versions/" + ours.Versions[0].ID + "/" + granted.URL
	rec := d.f.want(http.StatusOK, "", "GET", archive, nil, nil)
	if rec.Header().Get("Content-Type") != "application/zip" || rec.Header().Get("Content-Disposition") != layout.Attachment(oursName) {
		t.Fatalf("archive headers %v", rec.Header())
	}
	u, _ := url.Parse(archive)
	q := u.Query()
	q.Set("name", "Renamed.zip")
	u.RawQuery = q.Encode()
	d.f.want(http.StatusNotFound, "", "GET", u.String(), nil, nil)
	got, want := entries(t, rec.Body.Bytes()), entries(t, prepared[ours.ID])
	if len(got) != 8 || len(want) != 8 {
		t.Fatalf("%d pages, prepared %d", len(got), len(want))
	}
	for i := range got {
		page := i + 1
		marked := page == 3 || page == 5 || page == 7
		if suffix := fmt.Sprintf(" p%d", page); marked != bytes.HasSuffix(got[i], []byte(suffix)) {
			t.Fatalf("page %d marked: %v", page, !marked)
		}
		if !marked && !bytes.Equal(got[i], want[i]) || marked && !bytes.HasPrefix(got[i], want[i]) {
			t.Fatalf("page %d differs from the prepared archive", page)
		}
	}
	// Theirs (not in the group, ContentKit's name) and the free one (in it,
	// the policy's): the ordinary archive route, with reusable private caching.
	for c, name := range map[*catalog.Content]string{&theirs: "Theirs.zip", &free: "[Doujins.com] Free (en).zip"} {
		granted := d.file(d.premium, *c)
		rec := d.f.want(http.StatusOK, "", "GET", "/contents/"+c.ID+"/versions/"+c.Versions[0].ID+"/"+granted.URL, nil, nil)
		status, body, hdr := rec.Code, rec.Body.Bytes(), rec.Header()
		if status != http.StatusOK || !slices.EqualFunc(entries(t, body), entries(t, prepared[c.ID]), bytes.Equal) || granted.Size != 0 ||
			granted.Name != name || hdr.Get("Content-Disposition") != layout.Attachment(name) {
			t.Fatalf("%s: not the lazy archive: %d %+v", c.ID, status, granted)
		}
	}
	// Staff without Premium receive a target-only presigned capability. A
	// normal anchor transfer carries no SPA bearer header; it must still work.
	staffGrant := d.file(d.staff, theirs)
	staffArchive := "/contents/" + theirs.ID + "/versions/" + theirs.Versions[0].ID + "/" + staffGrant.URL
	staffResponse := d.f.want(http.StatusOK, "", "GET", staffArchive, nil, nil)
	if !slices.EqualFunc(entries(t, staffResponse.Body.Bytes()), entries(t, prepared[theirs.ID]), bytes.Equal) {
		t.Fatal("staff archive changed sources")
	}
	staffURL, _ := url.Parse(staffArchive)
	for key, value := range map[string]string{"editor": "false", "account": "foreign-account", "kind": "application", "revision": strings.Repeat("0", 64)} {
		bad := *staffURL
		query := bad.Query()
		query.Set(key, value)
		bad.RawQuery = query.Encode()
		d.f.want(http.StatusNotFound, "", "GET", bad.String(), nil, nil)
	}
	d.f.want(http.StatusNotFound, "", "GET", strings.Replace(staffArchive, theirs.ID, free.ID, 1), nil, nil)
	d.f.want(http.StatusNotFound, "", "GET", strings.Replace(staffArchive, theirs.Versions[0].ID, free.Versions[0].ID, 1), nil, nil)
	for _, target := range []struct{ table, id string }{{"content_versions", theirs.Versions[0].ID}, {"content_items", theirs.ID}} {
		table := pgx.Identifier{d.w.schema, target.table}.Sanitize()
		if _, err := d.w.pool.Exec(t.Context(), `UPDATE `+table+` SET deleted_at=now() WHERE tenant_id=$1 AND id=$2`, d.w.tenant, target.id); err != nil {
			t.Fatal(err)
		}
		d.f.want(http.StatusNotFound, "", "GET", staffArchive, nil, nil)
		if _, err := d.w.pool.Exec(t.Context(), `UPDATE `+table+` SET deleted_at=NULL WHERE tenant_id=$1 AND id=$2`, d.w.tenant, target.id); err != nil {
			t.Fatal(err)
		}
	}
	// A signed-out download of the free one is never marked.
	rec = d.f.want(http.StatusOK, "", "GET", "/contents/"+free.ID+"/versions/"+free.Versions[0].ID+"/"+d.file("", free).URL, nil, nil)
	if !slices.EqualFunc(entries(t, rec.Body.Bytes()), entries(t, prepared[free.ID]), bytes.Equal) {
		t.Fatal("anonymous download source bytes changed")
	}

	// The viewing token opens the web rendition, never the originals, the
	// full-resolution pages or the zip.
	ref, _ := d.w.reg.VersionRef("book", free.ID, free.Versions[0].ID)
	item, _ := d.w.reg.Item(ref)
	m, _, _ := d.w.jobs.Manifests().Get(t.Context(), ref)
	viewing := d.ring.Sign(token.ItemScope(ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version()), time.Now().Add(time.Hour))
	for p, want := range map[string]int{"read/001.webp": http.StatusOK, "pages/001.png": http.StatusNotFound,
		"high/001.webp": http.StatusNotFound, "download/pages.zip": http.StatusNotFound} {
		f, ok := m.Get(p)
		if !ok {
			t.Fatalf("no %s", p)
		}
		key, _ := item.Blob(f.Blob)
		if status, _, _ := d.fetch("https://media.test/v1/" + key + "?t=" + viewing); status != want {
			t.Fatalf("the viewing token on %s: %d, want %d", p, status, want)
		}
	}
}
