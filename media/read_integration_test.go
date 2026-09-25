package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
)

const readBase = "https://media.doujins.com"

var readKey = token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

// resolver is the host's ContentResolver: verdicts per content id.
type resolver struct {
	calls    atomic.Int32
	verdicts map[string]access.Resolution
	err      error
}

func (r *resolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = r.verdicts[ref.ContentID]
	}
	return out, nil
}

type readFixture struct {
	env      *s3test.Env
	kinds    *media.Registry
	ms       *media.Manifests
	res      *resolver
	gallery  contentref.ContentRef // versioned, 10 pages
	other    contentref.ContentRef // another gallery
	post     contentref.ContentRef // teaser + 2 files
	now      time.Time
	verifier token.Ring
}

func newReadFixture(t *testing.T) *readFixture {
	t.Helper()
	return newReadFixtureOn(t, s3test.Open(t))
}

func newReadFixtureOn(t *testing.T, env *s3test.Env) *readFixture {
	t.Helper()
	kinds, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Specs: map[string]media.Spec{"thumb": {Width: 460}, "high": {}}},
		media.Kind{Name: "post", Specs: map[string]media.Spec{"large": {}, "blurred": {Blur: 20}},
			Slots: map[string]media.Slot{"cover": {Aspect: media.Aspect1x1, Widths: []int{64}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	ring, err := token.NewRing(readKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &readFixture{env: env, kinds: kinds, ms: ms, res: &resolver{verdicts: map[string]access.Resolution{}},
		gallery: contentref.NewVersion(env.Tenant, "gallery", cid(1), "v1"),
		other:   contentref.NewVersion(env.Tenant, "gallery", cid(2), "v1"),
		post:    contentref.New(env.Tenant, "post", cid(501)),
		now:     time.Date(2026, 9, 23, 13, 7, 0, 0, time.UTC), verifier: ring}
	ctx := context.Background()
	for _, ref := range []contentref.ContentRef{f.gallery, f.other} {
		if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			for i := range 10 {
				name := fmt.Sprintf("%s-%03d.png", ref.ContentID, i)
				m.Files = append(m.Files, media.File{Name: name, Original: blobName("o" + name), Type: "image/png",
					Meta:     map[string]any{"w": 1200, "h": 1700 + i},
					Variants: map[string]media.Variant{"thumb": {Blob: blobName("t" + name)}, "high": {Blob: blobName("h" + name)}}})
			}
			m.Meta = map[string]any{"chapters": []any{map[string]any{"title": "Ch. 1", "start": 0, "end": 10}}}
			m.Downloads = map[string]media.Download{"zip": {Blob: blobName("zip" + ref.ContentID), Type: "application/zip", Size: 42}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ms.Edit(ctx, f.post, func(m *media.Manifest) error {
		m.Files = []media.File{
			{Name: "teaser", Original: blobName("teaser"), Type: "image/jpeg", Meta: map[string]any{"teaser": true},
				Variants: map[string]media.Variant{"blurred": {Blob: blobName("blurred")}}},
			{Name: "beach.jpg", Original: blobName("beach"), Type: "image/jpeg",
				Variants: map[string]media.Variant{"large": {Blob: blobName("beach-large")}}},
			{Name: "clip.mp4", Original: blobName("clip"), Type: "video/mp4", Meta: map[string]any{"duration": 12.5},
				HLS: &media.HLS{Source: blobName("clip"), Video: []media.Rendition{{Rung: 720, Width: 1280, Height: 720, Blob: blobName("clip-720")}}}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *readFixture) reader(t *testing.T, mode media.DeliveryMode, hooks media.Hooks) *media.Reader {
	t.Helper()
	r, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, Kinds: f.kinds, Resolver: f.res, Hooks: hooks,
		Delivery: media.Delivery{Mode: mode, BaseURL: readBase, CookieDomain: "doujins.com", SigningKey: readKey},
		Now:      func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *readFixture) read(t *testing.T, r *media.Reader, ref contentref.ContentRef, o media.ReadOptions) *media.ReadResult {
	t.Helper()
	f.res.calls.Store(0)
	out, err := r.Read(context.Background(), ref, access.Actor{ID: "u1"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.res.calls.Load(); n != 1 {
		t.Fatalf("resolver called %d times, want 1", n)
	}
	return out
}

// split parses a signed URL into its object key, token and dl name.
func split(t *testing.T, raw string) (key, tok, dl string) {
	t.Helper()
	if !strings.HasPrefix(raw, readBase+"/") {
		t.Fatalf("url %q not under %s", raw, readBase)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/"), u.Query().Get("t"), u.Query().Get("dl")
}

func (f *readFixture) key(t *testing.T, ref contentref.ContentRef, area, name string) string {
	t.Helper()
	item, err := f.kinds.Item(ref)
	if err != nil {
		t.Fatal(err)
	}
	switch area {
	case media.AreaOriginals:
		k, err := item.Original(name)
		if err != nil {
			t.Fatal(err)
		}
		return k
	case media.AreaManifest:
		return item.ManifestKey()
	}
	k, err := item.Private(name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// covers requires tok to open exactly the want keys among candidates.
func (f *readFixture) covers(t *testing.T, tok string, want map[string]bool, candidates ...string) {
	t.Helper()
	at := f.now.Add(time.Minute)
	for _, k := range candidates {
		err := f.verifier.Verify(tok, k, "", at)
		if want[k] && err != nil {
			t.Errorf("token should open %s: %v", k, err)
		}
		if !want[k] && err == nil {
			t.Errorf("token must not open %s", k)
		}
	}
}

func TestReadFullAccess(t *testing.T) {
	f := newReadFixture(t)
	f.res.verdicts[cid(1)] = access.Resolution{Visible: true, Accessible: true}
	page3 := cid(1) + "-003.png"
	thumb3 := f.key(t, f.gallery, media.AreaPrivate, blobName("t"+page3))
	high3 := f.key(t, f.gallery, media.AreaPrivate, blobName("h"+page3))
	forbidden := []string{
		f.key(t, f.gallery, media.AreaOriginals, blobName("o"+page3)),
		f.key(t, f.gallery, media.AreaManifest, ""),
		f.key(t, f.other, media.AreaPrivate, blobName("t"+cid(2)+"-003.png")),
	}

	t.Run("cookie", func(t *testing.T) {
		out := f.read(t, f.reader(t, media.DeliverCookie, media.Hooks{}), f.gallery,
			media.ReadOptions{Variants: []string{"thumb"}, Offset: 2, Limit: 3})
		if out.Access != media.AccessFull || out.Total != 10 || out.PreviewLimit != 0 || len(out.Files) != 10 {
			t.Fatalf("result %+v", out)
		}
		if out.Expires%int64(token.DefaultWindow/time.Second) != 0 || out.Expires <= f.now.Add(time.Hour).Unix() {
			t.Fatalf("expiry %d not window-aligned past now+ttl", out.Expires)
		}
		for i, fi := range out.Files {
			inRange := i >= 2 && i < 5
			if fi.Name != fmt.Sprintf("%s-%03d.png", cid(1), i) || fi.Locked || fi.Width != 1200 || fi.Height != 1700+i {
				t.Fatalf("file %d: %+v", i, fi)
			}
			if inRange != (fi.URL != "") {
				t.Fatalf("file %d url %q; want url only in [2,5)", i, fi.URL)
			}
		}
		key, tok, _ := split(t, out.Files[3].URL)
		if key != thumb3 || tok != "" {
			t.Fatalf("cookie mode url must be plain: %s", out.Files[3].URL)
		}
		c := out.Cookie
		if c == nil || c.Name != "mt" || c.Domain != "doujins.com" || c.Path != "/"+f.env.Tenant+"/gallery/"+cid(1)+"/private/" ||
			!c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || !c.Expires.Equal(time.Unix(out.Expires, 0)) {
			t.Fatalf("cookie %+v", c)
		}
		f.covers(t, c.Value, map[string]bool{thumb3: true, high3: true}, append([]string{thumb3, high3}, forbidden...)...)
		if len(out.Downloads) != 1 || out.Downloads[0].Name != cid(1)+"-zip.zip" {
			t.Fatalf("downloads %+v", out.Downloads)
		}
	})

	t.Run("url", func(t *testing.T) {
		out := f.read(t, f.reader(t, media.DeliverURL, media.Hooks{}), f.gallery,
			media.ReadOptions{Variants: []string{"high"}, Limit: 4})
		if out.Cookie != nil {
			t.Fatal("url mode must not set a cookie")
		}
		key, tok, _ := split(t, out.Files[3].URL)
		if key != high3 || tok == "" {
			t.Fatalf("url mode needs ?t=: %s", out.Files[3].URL)
		}
		if _, tok0, _ := split(t, out.Files[0].URL); tok0 != tok {
			t.Fatal("full access uses one folder token")
		}
		f.covers(t, tok, map[string]bool{thumb3: true, high3: true}, append([]string{thumb3, high3}, forbidden...)...)
		if out.Files[4].URL != "" {
			t.Fatal("url past limit")
		}
	})
}

// TestReadWithoutConditionalPut reads manifests edited under the PGLocker, as
// on Ceph RGW.
func TestReadWithoutConditionalPut(t *testing.T) {
	f := newReadFixtureOn(t, s3test.Open(t).WithoutConditionalPut(t))
	f.res.verdicts[cid(1)] = access.Resolution{Visible: true, Accessible: true}
	out := f.read(t, f.reader(t, media.DeliverURL, media.Hooks{}), f.gallery, media.ReadOptions{Variants: []string{"thumb"}, Limit: 10})
	if out.Access != media.AccessFull || out.Total != 10 || len(out.Files) != 10 || out.Files[9].URL == "" {
		t.Fatalf("result %+v", out)
	}
}

func TestReadPreview(t *testing.T) {
	f := newReadFixture(t)
	for name, v := range map[string]access.Resolution{
		"free preview":       {Visible: true, PreviewLimit: 3},
		"scheduled chapters": {Visible: true, Accessible: true, PreviewLimit: 3},
	} {
		t.Run(name, func(t *testing.T) {
			f.res.verdicts[cid(1)] = v
			// Cookie mode still gets per-file URL tokens for preview viewers.
			out := f.read(t, f.reader(t, media.DeliverCookie, media.Hooks{}), f.gallery,
				media.ReadOptions{Variants: []string{"high"}, Offset: 1, Limit: 50})
			if out.Access != media.AccessPreview || out.PreviewLimit != 3 || out.Cookie != nil || out.Downloads != nil {
				t.Fatalf("result %+v", out)
			}
			for i, fi := range out.Files {
				if i < 3 {
					if fi.Locked || fi.Name == "" || (i == 0) != (fi.URL == "") {
						t.Fatalf("file %d: %+v", i, fi)
					}
					continue
				}
				if !fi.Locked || fi.Name != "" || fi.URL != "" || fi.Type != "image/png" || fi.Width != 1200 || fi.Height != 1700+i {
					t.Fatalf("hidden file %d must expose type/dimensions only: %+v", i, fi)
				}
			}
			body, _ := json.Marshal(out)
			if strings.Contains(string(body), cid(1)+"-003.png") || strings.Contains(string(body), "zip") {
				t.Fatalf("response leaks hidden names or downloads: %s", body)
			}
			key1, tok1, _ := split(t, out.Files[1].URL)
			key2, _, _ := split(t, out.Files[2].URL)
			f.covers(t, tok1, map[string]bool{key1: true}, key1, key2,
				f.key(t, f.gallery, media.AreaPrivate, blobName("t"+cid(1)+"-001.png")),
				f.key(t, f.gallery, media.AreaPrivate, blobName("h"+cid(1)+"-003.png")),
				f.key(t, f.gallery, media.AreaOriginals, blobName("o"+cid(1)+"-001.png")),
				f.key(t, f.other, media.AreaPrivate, blobName("h"+cid(2)+"-001.png")))
		})
	}
}

func TestReadTeaserAndDeny(t *testing.T) {
	f := newReadFixture(t)
	r := f.reader(t, media.DeliverCookie, media.Hooks{})
	ctx := context.Background()

	f.res.verdicts[cid(501)] = access.Resolution{Visible: true}
	out := f.read(t, r, f.post, media.ReadOptions{Variants: []string{"large", "blurred"}})
	if out.Access != media.AccessNone || out.Cookie != nil || len(out.Files) != 3 {
		t.Fatalf("result %+v", out)
	}
	teaser, beach, clip := out.Files[0], out.Files[1], out.Files[2]
	if teaser.Name != "teaser" || !teaser.Teaser || teaser.Variant != "blurred" || teaser.URL == "" {
		t.Fatalf("teaser %+v", teaser)
	}
	if !beach.Locked || beach.Name != "" || beach.URL != "" || !clip.Locked || !clip.HLS || clip.Duration != 12.5 {
		t.Fatalf("locked files %+v %+v", beach, clip)
	}
	key, tok, _ := split(t, teaser.URL)
	f.covers(t, tok, map[string]bool{key: true}, key,
		f.key(t, f.post, media.AreaPrivate, blobName("beach-large")),
		f.key(t, f.post, media.AreaOriginals, blobName("teaser")))

	g, err := r.Grant(ctx, f.post, access.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		i    int
		blob string
	}{{1, blobName("beach-large")}, {0, blobName("teaser")}, {0, blobName("beach-large")}} {
		if _, err := g.URL(c.i, c.blob); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("URL(%d, %s) = %v, want ErrNotAllowed", c.i, c.blob, err)
		}
	}
	if _, _, err := g.DownloadURL(ctx, "zip"); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatal("downloads need full access")
	}

	for name, c := range map[string]struct {
		verdict access.Resolution
		err     error
		want    error
	}{
		"invisible":           {verdict: access.Resolution{Accessible: true}, want: media.ErrNotVisible},
		"resolver error":      {verdict: access.Resolution{Visible: true, Accessible: true}, err: errors.New("db down"), want: media.ErrResolve},
		"foreign tenant ref":  {verdict: access.Resolution{Visible: true, Accessible: true, Ref: contentref.New("x", "post", cid(501))}, want: media.ErrResolve},
		"visible, no preview": {verdict: access.Resolution{Visible: true}},
	} {
		t.Run(name, func(t *testing.T) {
			f.res.verdicts[cid(1)], f.res.err = c.verdict, c.err
			out, err := r.Read(ctx, f.gallery, access.Actor{}, media.ReadOptions{Variants: []string{"high"}})
			if c.want != nil {
				if !errors.Is(err, c.want) || out != nil {
					t.Fatalf("got %v, %+v; want %v", out, err, c.want)
				}
				return
			}
			if err != nil || out.Access != media.AccessNone || out.Total != 10 {
				t.Fatalf("got %+v, %v", out, err)
			}
			for _, fi := range out.Files {
				if !fi.Locked || fi.URL != "" || fi.Name != "" {
					t.Fatalf("file %+v", fi)
				}
			}
		})
	}
	f.res.err = nil
}

func TestReadDownloadNames(t *testing.T) {
	f := newReadFixture(t)
	f.res.verdicts[cid(1)] = access.Resolution{Visible: true, Accessible: true}
	const name = "[Artist] タイトル (English).zip"
	r := f.reader(t, media.DeliverCookie, media.Hooks{DownloadName: func(_ context.Context, ref contentref.ContentRef, key string, d media.Download) (string, error) {
		if ref.ContentID != cid(1) || key != "zip" || d.Type != "application/zip" {
			return "", fmt.Errorf("unexpected download %s %s %+v", ref, key, d)
		}
		return name, nil
	}})
	out := f.read(t, r, f.gallery, media.ReadOptions{})
	if len(out.Downloads) != 1 {
		t.Fatalf("downloads %+v", out.Downloads)
	}
	d := out.Downloads[0]
	key, tok, dl := split(t, d.URL)
	at := f.now.Add(time.Minute)
	if d.Name != name || dl != name || d.Size != 42 || key != f.key(t, f.gallery, media.AreaPrivate, blobName("zip"+cid(1))) {
		t.Fatalf("download %+v (dl %q)", d, dl)
	}
	if err := f.verifier.Verify(tok, key, dl, at); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"other.zip", ""} {
		if f.verifier.Verify(tok, key, bad, at) == nil {
			t.Fatalf("download token must not verify with dl=%q", bad)
		}
	}
	if f.verifier.Verify(tok, f.key(t, f.gallery, media.AreaPrivate, blobName("h"+cid(1)+"-000.png")), dl, at) == nil {
		t.Fatal("download token opened another blob")
	}
	for _, fi := range out.Files {
		if fi.URL != "" {
			t.Fatal("no variant requested: metadata only")
		}
	}
}

func TestReadHandler(t *testing.T) {
	f := newReadFixture(t)
	r := f.reader(t, media.DeliverCookie, media.Hooks{})
	srv := httptest.NewServer(http.StripPrefix("/media", r.Handler(media.HandlerOptions{Tenant: f.env.Tenant})))
	defer srv.Close()
	get := func(path string) (*http.Response, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp, body
	}

	f.res.verdicts[cid(1)] = access.Resolution{Visible: true, Accessible: true}
	resp, body := get("/media/gallery/" + cid(1) + "@v1?variant=thumb&offset=0&limit=2")
	if resp.StatusCode != 200 || body["access"] != "full" || body["total"] != float64(10) || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"mt=k1.", "Domain=doujins.com", "Path=/" + f.env.Tenant + "/gallery/" + cid(1) + "/private/", "Max-Age=", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	files := body["files"].([]any)
	if len(files) != 10 || files[1].(map[string]any)["url"] == nil || files[2].(map[string]any)["url"] != nil {
		t.Fatalf("files %v", files)
	}

	f.res.verdicts[cid(1)] = access.Resolution{Visible: true, PreviewLimit: 3}
	resp, _ = get("/media/gallery/" + cid(1) + "@v1?variant=thumb")
	if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("preview: %d, cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}

	for path, want := range map[string]int{
		"/media/gallery/" + cid(1):                       400, // versioned kind needs @version
		"/media/gallery/" + cid(1) + "@v1?limit=x":       400,
		"/media/unknown/" + cid(1):                       404,
		"/media/gallery/" + cid(9) + "@v1?variant=thumb": 404, // resolver: not visible
		"/media/gallery/1@v1?variant=thumb":              404, // not a content id
	} {
		if resp, body := get(path); resp.StatusCode != want || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("%s: %d %v, want %d", path, resp.StatusCode, body, want)
		}
	}
	f.res.err = errors.New("boom")
	if resp, body := get("/media/gallery/" + cid(1) + "@v1?variant=thumb"); resp.StatusCode != 500 || body["code"] != "internal_error" {
		t.Fatalf("resolver error must deny: %d %v", resp.StatusCode, body)
	}

	if resp, _ := get("/media/post/" + cid(501) + "/slots/cover"); resp.StatusCode != 500 {
		t.Fatalf("slot read must resolve: %d", resp.StatusCode)
	}
	f.res.err = nil
	hidden := f.res.verdicts[cid(501)]
	f.res.verdicts[cid(501)] = access.Resolution{}
	if resp, _ := get("/media/post/" + cid(501) + "/slots/cover"); resp.StatusCode != 404 {
		t.Fatalf("slot of a hidden item: %d", resp.StatusCode)
	}
	f.res.verdicts[cid(501)] = access.Resolution{Visible: true}
	defer func() { f.res.verdicts[cid(501)] = hidden }()
	if resp, body := get("/media/post/" + cid(501) + "/slots/cover"); resp.StatusCode != 200 || body["pending"] != false || len(body["outputs"].([]any)) != 0 {
		t.Fatalf("uncommitted slot: %d %v", resp.StatusCode, body)
	}
	spec := media.Slot{Aspect: media.Aspect1x1, Widths: []int{64}}
	rec := media.SlotRecord{Original: blobName("cover-original"), Edit: &media.Edit{Crop: &media.Crop{X: 10, Y: 10, W: 100, H: 100}}}
	fp := rec.Fingerprint(spec)
	rec.Result = &media.SlotResult{Of: fp, Source: rec.Original, Dims: media.Dims{W: 200, H: 200},
		Outputs: []media.SlotRendition{{Rung: 64, W: 64, H: 64, Blob: blobName("cover-64")}}}
	if err := f.ms.UpdateSlot(context.Background(), f.post, "cover", func(r *media.SlotRecord) error { *r = rec; return nil }); err != nil {
		t.Fatal(err)
	}
	cover := readBase + "/" + f.env.Tenant + "/post/" + cid(501) + "/public/" + blobName("cover-64")
	resp, body = get("/media/post/" + cid(501) + "/slots/cover")
	if outs := body["outputs"].([]any); resp.StatusCode != 200 || body["pending"] != false || len(outs) != 1 || body["aspect"] != "1:1" ||
		outs[0].(map[string]any)["url"] != cover || body["dims"].(map[string]any)["w"] != float64(200) || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("slot: %d %v", resp.StatusCode, body)
	}
	if resp, _ := get("/media/post/" + cid(501) + "/slots/nope"); resp.StatusCode != 404 {
		t.Fatalf("unknown slot: %d", resp.StatusCode)
	}
	listing := rec.Result.Listing(spec)
	if listed, err := r.ListedSlot(f.post, "cover", listing); err != nil || len(listed.Outputs) != 1 || listed.Outputs[0].URL != cover ||
		listed.Outputs[0].H != 64 || listed.Aspect != media.Aspect1x1 {
		t.Fatalf("listed slot %+v %v", listed, err)
	}

	// A hidden item's cover is listed to no viewer.
	if _, err := f.ms.EditRoot(context.Background(), f.post, func(r *media.Root) error { r.Hidden = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp, body := get("/media/post/" + cid(501) + "/slots/cover"); resp.StatusCode != 200 || len(body["outputs"].([]any)) != 0 {
		t.Fatalf("hidden slot: %d %v", resp.StatusCode, body)
	}
}

func TestReadEditorOnlyVariants(t *testing.T) {
	f := newReadFixture(t)
	ref := contentref.New(f.env.Tenant, "post", cid(502))
	edit := &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 100, H: 100}}
	if _, err := f.ms.Edit(context.Background(), ref, func(m *media.Manifest) error {
		m.Files = []media.File{{Name: "a.png", Original: blobName("a"), Type: "image/png", Edit: edit, Dims: &media.Dims{W: 400, H: 200},
			Variants: map[string]media.Variant{"large": {Blob: blobName("a-large")}, "editor": {Blob: blobName("a-editor"), Editor: true}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	editorKey := f.key(t, ref, media.AreaPrivate, blobName("a-editor"))
	largeKey := f.key(t, ref, media.AreaPrivate, blobName("a-large"))
	for _, mode := range []media.DeliveryMode{media.DeliverCookie, media.DeliverURL} {
		for _, res := range []access.Resolution{
			{Visible: true, Accessible: true},
			{Visible: true, PreviewLimit: 1},
			{Visible: true, Accessible: true, Editor: true},
			{Visible: true, PreviewLimit: 1, Editor: true},
		} {
			t.Run(fmt.Sprintf("%s %+v", mode, res), func(t *testing.T) {
				f.res.verdicts[cid(502)] = res
				r := f.reader(t, mode, media.Hooks{})
				out := f.read(t, r, ref, media.ReadOptions{Variants: []string{"editor", "large"}})
				fi := out.Files[0]
				want := "large"
				if res.Editor {
					want = "editor"
				}
				if fi.Variant != want || (fi.Edit != nil) != res.Editor || (fi.Dims != nil) != res.Editor {
					t.Fatalf("file %+v", fi)
				}
				g, err := r.Grant(context.Background(), ref, access.Actor{ID: "u1"})
				if err != nil {
					t.Fatal(err)
				}
				// EditorOnly renditions live in private/ like the rest; only
				// editors get them signed.
				viewer, err := g.URL(0, blobName("a-large"))
				if err != nil {
					t.Fatal(err)
				}
				_, tok, _ := split(t, viewer)
				if c := g.Cookie(); c != nil {
					tok = c.Value
				}
				f.covers(t, tok, map[string]bool{largeKey: true}, largeKey)
				u, err := g.URL(0, blobName("a-editor"))
				if !res.Editor {
					if !errors.Is(err, media.ErrNotAllowed) {
						t.Fatalf("editor blob signed for a viewer: %q %v", u, err)
					}
					return
				}
				key, tok, _ := split(t, u)
				if err != nil || key != editorKey {
					t.Fatalf("editor url %q %v", u, err)
				}
				if c := g.Cookie(); c != nil {
					tok = c.Value
				}
				f.covers(t, tok, map[string]bool{editorKey: true}, editorKey)
			})
		}
	}
}
