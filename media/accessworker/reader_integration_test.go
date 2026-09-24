package accessworker_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/accessworker"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
)

type verdicts map[string]access.Resolution

func (v verdicts) Resolve(_ context.Context, ref contentref.ContentRef, _ access.Actor) (access.Resolution, error) {
	return v[ref.ContentID], nil
}

// TestReaderThroughWorker serves media.Reader output (URLs, download links
// and the folder cookie) through the real worker over the bucket.
func TestReaderThroughWorker(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	kinds, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Specs: map[string]media.Spec{"thumb": {Width: 460}}},
		media.Kind{Name: "post", Specs: map[string]media.Spec{"blurred": {Blur: 20}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	put := func(key, body string) {
		t.Helper()
		if _, err := env.Store.Put(ctx, key, strings.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/webp"}); err != nil {
			t.Fatal(err)
		}
	}
	full := contentref.NewVersion(env.Tenant, "gallery", "1", "v1")
	preview := contentref.NewVersion(env.Tenant, "gallery", "2", "v1")
	post := contentref.New(env.Tenant, "post", "501")
	content := map[string]string{} // blob key -> body
	for _, ref := range []contentref.ContentRef{full, preview} {
		item, _ := kinds.Item(ref)
		if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			for i := range 4 {
				name := ref.ContentID + "-" + string(rune('a'+i))
				m.Files = append(m.Files, media.File{Name: name + ".png", Original: sha("o" + name), Type: "image/png",
					Variants: map[string]media.Variant{"thumb": {Blob: sha("t" + name)}}})
				put(item.OriginalsPrefix()+sha("o"+name), "original "+name)
				k, _ := item.Blob(sha("t" + name))
				content[k] = "thumb " + name
				put(k, content[k])
			}
			m.Downloads = map[string]media.Download{"zip": {Blob: sha("zip" + ref.ContentID), Type: "application/zip"}}
			k, _ := item.Blob(sha("zip" + ref.ContentID))
			put(k, "zip "+ref.ContentID)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	postItem, _ := kinds.Item(post)
	if _, err := ms.Edit(ctx, post, func(m *media.Manifest) error {
		m.Files = []media.File{
			{Name: "teaser", Original: sha("teaser"), Meta: map[string]any{"teaser": true},
				Variants: map[string]media.Variant{"blurred": {Blob: sha("blurred")}}},
			{Name: "locked", Original: sha("locked"), Variants: map[string]media.Variant{"blurred": {Blob: sha("locked-b")}}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"blurred", "locked-b"} {
		k, _ := postItem.Blob(sha(n))
		put(k, n)
	}

	key := token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}
	h, err := accessworker.New(accessworker.Config{
		Endpoint: env.Config.Endpoint, Bucket: env.Config.Bucket, Region: env.Config.Region,
		AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Ring: mustRing(t, key, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := httptest.NewTLSServer(h) // the cookie is Secure
	t.Cleanup(worker.Close)
	base, _ := url.Parse(worker.URL)

	res := verdicts{
		"1":   {Visible: true, Accessible: true},
		"2":   {Visible: true, PreviewLimit: 2},
		"501": {Visible: true},
	}
	reader := func(mode media.DeliveryMode) *media.Reader {
		r, err := media.NewReader(media.ReaderOptions{Manifests: ms, Kinds: kinds, Resolver: res,
			Delivery: media.Delivery{Mode: mode, BaseURL: worker.URL, CookieDomain: base.Hostname(), SigningKey: key}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	read := func(r *media.Reader, ref contentref.ContentRef) *media.ReadResult {
		out, err := r.Read(ctx, ref, access.Actor{ID: "u1"}, media.ReadOptions{Variants: []string{"thumb", "blurred"}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	get := func(c *http.Client, u string) (int, string, http.Header) {
		t.Helper()
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	bare := worker.Client()
	keyOf := func(u string) string {
		p, _ := url.Parse(u)
		return strings.TrimPrefix(p.Path, "/")
	}
	fullItem, _ := kinds.Item(full)

	t.Run("cookie mode", func(t *testing.T) {
		out := read(reader(media.DeliverCookie), full)
		if out.Cookie == nil {
			t.Fatal("full access in cookie mode sets the folder cookie")
		}
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(base, []*http.Cookie{out.Cookie})
		c := &http.Client{Transport: worker.Client().Transport, Jar: jar}
		for _, f := range out.Files {
			if strings.Contains(f.URL, "?") {
				t.Fatalf("cookie mode URL carries a token: %s", f.URL)
			}
			if st, body, hdr := get(c, f.URL); st != 200 || body != content[keyOf(f.URL)] || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
				t.Fatalf("%s: %d %q", f.URL, st, body)
			}
			if st, _, _ := get(bare, f.URL); st != 403 {
				t.Fatalf("without the cookie: %d", st)
			}
		}
		orig := worker.URL + "/" + fullItem.OriginalsPrefix() + sha("o1-a")
		if cs := jar.Cookies(mustURL(t, orig)); len(cs) != 0 {
			t.Fatal("cookie sent outside its blobs/ path")
		}
		if st, _, _ := get(c, orig); st != 404 {
			t.Fatalf("original through worker: %d", st)
		}
		manifest, _ := fullItem.ManifestKey()
		if st, _, _ := get(c, worker.URL+"/"+manifest); st != 404 {
			t.Fatalf("manifest through worker: %d", st)
		}
		other := read(reader(media.DeliverURL), preview).Files[0].URL
		if st, _, _ := get(c, strings.Split(other, "?")[0]); st != 403 {
			t.Fatalf("cookie opened another item: %d", st)
		}
		if len(out.Downloads) != 1 {
			t.Fatalf("downloads: %+v", out.Downloads)
		}
		d := out.Downloads[0]
		st, body, hdr := get(bare, d.URL)
		if st != 200 || body != "zip 1" || hdr.Get("Content-Disposition") != token.Attachment(d.Name) {
			t.Fatalf("download: %d %q %v", st, body, hdr)
		}
	})

	t.Run("URL mode full access", func(t *testing.T) {
		out := read(reader(media.DeliverURL), full)
		if out.Cookie != nil {
			t.Fatal("URL mode sets no cookie")
		}
		for _, f := range out.Files {
			if st, body, _ := get(bare, f.URL); st != 200 || body != content[keyOf(f.URL)] {
				t.Fatalf("%s: %d %q", f.URL, st, body)
			}
		}
		u := mustURL(t, out.Files[0].URL)
		u.Path = "/" + fullItem.OriginalsPrefix() + sha("o1-a")
		if st, _, _ := get(bare, u.String()); st != 404 {
			t.Fatalf("folder token on originals: %d", st)
		}
	})

	t.Run("preview gets per-file URL tokens", func(t *testing.T) {
		out := read(reader(media.DeliverCookie), preview)
		if out.Cookie != nil || out.PreviewLimit != 2 {
			t.Fatalf("preview: cookie %v, limit %d", out.Cookie, out.PreviewLimit)
		}
		for i, f := range out.Files {
			if i >= 2 {
				if f.URL != "" || !f.Locked {
					t.Fatalf("file %d past the cut: %+v", i, f)
				}
				continue
			}
			if st, body, _ := get(bare, f.URL); st != 200 || body != content[keyOf(f.URL)] {
				t.Fatalf("%s: %d %q", f.URL, st, body)
			}
		}
		item, _ := kinds.Item(preview)
		hidden, _ := item.Blob(sha("t2-c"))
		u := mustURL(t, out.Files[0].URL)
		u.Path = "/" + hidden
		if st, _, _ := get(bare, u.String()); st != 403 {
			t.Fatalf("per-file token opened a page past the cut: %d", st)
		}
		if len(out.Downloads) != 0 {
			t.Fatal("preview offers no downloads")
		}
	})

	t.Run("teaser only", func(t *testing.T) {
		out := read(reader(media.DeliverCookie), post)
		if out.Files[0].URL == "" || out.Files[1].URL != "" {
			t.Fatalf("teaser: %+v", out.Files)
		}
		if st, body, _ := get(bare, out.Files[0].URL); st != 200 || body != "blurred" {
			t.Fatalf("teaser: %d %q", st, body)
		}
		locked, _ := postItem.Blob(sha("locked-b"))
		u := mustURL(t, out.Files[0].URL)
		u.Path = "/" + locked
		if st, _, _ := get(bare, u.String()); st != 403 {
			t.Fatalf("teaser token opened a locked file: %d", st)
		}
	})
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
