package accessworker_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/accessworker"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// visibility answers the anonymous and viewer verdict it holds; actor "editor"
// is an editor of any visible item.
type visibility struct {
	mu  sync.Mutex
	res access.Resolution
}

func (v *visibility) set(r access.Resolution) { v.mu.Lock(); v.res = r; v.mu.Unlock() }

func (v *visibility) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	r := v.res
	r.Editor = a.ID == "editor"
	if r.Editor {
		r.Visible = true
	}
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = r
	}
	return out, nil
}

type actorHeader struct{}

func (actorHeader) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorHeader{}).(access.Actor)
	return a, ok
}

// TestExposure serves a video item's poster through the real worker as its
// visibility changes: nothing public for a draft (editors read private/),
// the public/ copy for a paid or free item, gone at once when hidden.
// Every viewer of the poster gets its frame time.
func TestExposure(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	kinds, err := media.NewRegistry(media.Kind{Name: "clip", Video: &media.Video{PosterWidths: []int{480, 960, 1920}}, Types: []string{"video/mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "clip", cid(7))
	item, _ := kinds.Item(ref)
	vis := &visibility{}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: kinds, Resolver: vis, Locker: s3test.Locker(t, env.Store)})
	if err != nil {
		t.Fatal(err)
	}

	// What the image job renders: a private/ output and the record listing it.
	blob := sha("poster")
	private, _ := item.Private(blob)
	orig := sha("frame")
	if err := ms.UpdateSlot(ctx, ref, media.PosterSlot, func(r *media.SlotRecord) error {
		r.Original = orig
		r.Frame = &media.PosterFrame{File: "source", Time: 2.5, Auto: true, Source: orig}
		fp := r.Fingerprint((&media.Video{PosterWidths: []int{480, 960, 1920}}).Poster())
		r.Result = &media.SlotResult{Of: fp, Source: r.Original, Outputs: []media.SlotRendition{{Rung: 480, W: 480, H: 270, Blob: blob}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.Put(ctx, private, strings.NewReader("poster"), 6, media.PutOptions{ContentType: "image/webp"}); err != nil {
		t.Fatal(err)
	}

	worker, err := accessworker.New(accessworker.Config{Endpoint: env.Config.Endpoint, Bucket: env.Config.Bucket, Region: env.Config.Region,
		AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Ring: mustRing(t, k2, nil)})
	if err != nil {
		t.Fatal(err)
	}
	mediaSrv := httptest.NewServer(worker)
	defer mediaSrv.Close()
	reader, err := media.NewReader(media.ReaderOptions{Manifests: ms, Kinds: kinds, Resolver: vis,
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: mediaSrv.URL, SigningKey: k2}})
	if err != nil {
		t.Fatal(err)
	}
	api := reader.Handler(media.HandlerOptions{Tenant: env.Tenant, Identity: actorHeader{}, Limit: media.ViewerLimit{Disabled: true}})
	images := func(actor string) (int, media.VideoImages) {
		t.Helper()
		req := httptest.NewRequest("GET", "/clip/"+cid(7)+"/video-images", nil)
		if actor != "" {
			req = req.WithContext(context.WithValue(req.Context(), actorHeader{}, access.Actor{ID: actor}))
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		var v media.VideoImages
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	fetch := func(u string) (int, string) {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Cache-Control")
	}
	publicKey, _ := item.Public(blob)
	public := func() int { code, _ := fetch(mediaSrv.URL + "/" + publicKey); return code }
	expose := func(res access.Resolution) {
		t.Helper()
		vis.set(res)
		if err := jobs.Expose(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{private, item.ManifestKey(), item.OriginalsPrefix() + orig} {
		if code, _ := fetch(mediaSrv.URL + "/" + k); code != http.StatusNotFound {
			t.Fatalf("%s without a token: %d", k, code)
		}
	}

	// A draft: nothing public, the read API hides it, editors see private/.
	expose(access.Resolution{})
	if code := public(); code != http.StatusNotFound {
		t.Fatalf("draft poster: %d", code)
	}
	if code, _ := images(""); code != http.StatusNotFound {
		t.Fatalf("draft video-images: %d", code)
	}
	code, ed := images("editor")
	if code != http.StatusOK || len(ed.Poster.Outputs) != 1 {
		t.Fatalf("editor video-images: %d %+v", code, ed)
	}
	if u := ed.Poster.Outputs[0].URL; !strings.Contains(u, "/private/"+blob+"?t=") {
		t.Fatalf("editor url %s", u)
	} else if code, cc := fetch(u); code != http.StatusOK || cc != "private, max-age=31536000, immutable" {
		t.Fatalf("editor fetch %d %q", code, cc)
	}

	// Paid: the poster is public.
	expose(access.Resolution{Visible: true})
	if code, cc := fetch(mediaSrv.URL + "/" + publicKey); code != http.StatusOK || cc != "public, max-age=31536000, immutable" {
		t.Fatalf("paid poster: %d %q", code, cc)
	}
	code, v := images("viewer")
	if code != http.StatusOK || len(v.Poster.Outputs) != 1 || v.Poster.Outputs[0].URL != mediaSrv.URL+"/"+publicKey ||
		v.Poster.File != "source" || v.Poster.Time == nil || *v.Poster.Time != 2.5 || v.Poster.Selection != nil {
		t.Fatalf("paid video-images: %d %+v", code, v)
	}

	// Free: the same public copy.
	expose(access.Resolution{Visible: true, Accessible: true})
	if code, v := images(""); code != http.StatusOK || public() != http.StatusOK || len(v.Poster.Outputs) != 1 {
		t.Fatalf("free video-images: %d %+v", code, v)
	}

	// Hidden (unpublished, deleted): gone at once; private/ is untouched.
	expose(access.Resolution{})
	if code := public(); code != http.StatusNotFound {
		t.Fatalf("hidden poster: %d", code)
	}
	if code, _ := images("viewer"); code != http.StatusNotFound {
		t.Fatalf("hidden video-images: %d", code)
	}
	if code, ed := images("editor"); code != http.StatusOK || len(ed.Poster.Outputs) != 1 {
		t.Fatalf("editor after hiding: %d %+v", code, ed)
	}
}

// TestViewerRateLimit caps each viewer's read API requests; other viewers
// are unaffected, and the bucket refills.
func TestViewerRateLimit(t *testing.T) {
	env := s3test.Open(t)
	kinds, err := media.NewRegistry(media.Kind{Name: "post", Specs: map[string]media.Spec{"large": {}}})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	reader, err := media.NewReader(media.ReaderOptions{Manifests: ms, Kinds: kinds,
		Resolver: verdicts{cid(1): {Visible: true, Accessible: true}},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.example", SigningKey: k2}})
	if err != nil {
		t.Fatal(err)
	}
	api := reader.Handler(media.HandlerOptions{Tenant: env.Tenant, Identity: actorHeader{}, Limit: media.ViewerLimit{PerSecond: 5, Burst: 3}})
	get := func(actor, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if actor != "" {
			req = req.WithContext(context.WithValue(req.Context(), actorHeader{}, access.Actor{ID: actor}))
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}
	for i := range 3 {
		if rec := get("scraper", "/post/"+cid(1)); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := get("scraper", "/post/"+cid(1)+"/hls/a/master.m3u8")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("over the limit: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := get("someone-else", "/post/"+cid(1)); rec.Code != http.StatusOK {
		t.Fatalf("another viewer: %d", rec.Code)
	}
	for i := range 3 {
		if rec := get("", "/post/"+cid(1)); rec.Code != http.StatusOK {
			t.Fatalf("anonymous %d: %d", i, rec.Code)
		}
	}
	if rec := get("", "/post/"+cid(1)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("anonymous from one address over the limit: %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if rec := get("scraper", "/post/"+cid(1)); rec.Code != http.StatusOK {
		t.Fatalf("after refill: %d", rec.Code)
	}
}
