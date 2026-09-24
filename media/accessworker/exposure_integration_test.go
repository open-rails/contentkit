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

// TestVideoExposure publishes a video item's poster and hover preview per
// its visibility and serves them through the real worker: nothing for a
// draft, the poster alone for a paid item, both for a free one, nothing
// again once unpublished; editors see the unpublished outputs.
func TestVideoExposure(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	kinds, err := media.NewRegistry(media.Kind{Name: "clip", Video: &media.Video{PosterWidths: []int{480, 960, 1920}}, Types: []string{"video/mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "clip", "7")
	item, _ := kinds.Item(ref)
	vis := &visibility{}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: kinds, Resolver: vis})
	if err != nil {
		t.Fatal(err)
	}

	// What the image job and the video worker render: editor/ outputs, plus
	// the records that list them.
	poster, _ := item.SlotOutput(media.PosterSlot, 480)
	outputs := map[string]string{poster: "poster", item.HoverPreviewOutput(320, true): "clip mp4", item.HoverPreviewOutput(320, false): "clip webp"}
	for k, body := range outputs {
		if !strings.HasPrefix(k, item.EditorPrefix()) {
			t.Fatalf("rendered output outside editor/: %s", k)
		}
		if _, err := env.Store.Put(ctx, k, strings.NewReader(body), int64(len(body)),
			media.PutOptions{ContentType: "image/webp", CacheControl: "no-cache", Metadata: map[string]string{"of": "v1"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.UpdateSlot(ctx, ref, media.PosterSlot, func(r *media.SlotRecord) error {
		r.Original = `"etag"`
		fp := r.Fingerprint((&media.Video{PosterWidths: []int{480, 960, 1920}}).Poster())
		r.Result = &media.SlotResult{Of: fp, Version: fp, Source: r.Original, Outputs: []media.Dims{{W: 480, H: 270}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ms.UpdateHoverPreview(ctx, ref, func(*media.HoverPreviewRecord) (*media.HoverPreviewRecord, error) {
		rec := &media.HoverPreviewRecord{File: "source", Start: 1, Duration: 3}
		rec.Result = &media.HoverPreviewResult{Of: rec.Key(), Version: "v1", Outputs: []media.Dims{{W: 320, H: 180}}}
		return rec, nil
	}); err != nil {
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
		req := httptest.NewRequest("GET", "/clip/7/video-images", nil)
		if actor != "" {
			req = req.WithContext(context.WithValue(req.Context(), actorHeader{}, access.Actor{ID: actor}))
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		var v media.VideoImages
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	fetch := func(u string) int {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	public := func(key string) int { return fetch(mediaSrv.URL + "/" + key) }
	posterURL, _ := item.SlotPublic(media.PosterSlot, 480)
	mp4URL, webpURL := item.HoverPreviewPublic(320, true), item.HoverPreviewPublic(320, false)
	publish := func(res access.Resolution) {
		t.Helper()
		vis.set(res)
		if err := jobs.Publish(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	for k := range outputs {
		if code := public(k); code != http.StatusForbidden {
			t.Fatalf("editor/ output %s without a token: %d", k, code)
		}
	}

	// A draft: nothing public, the read API hides it, editors see everything.
	publish(access.Resolution{})
	for _, k := range []string{posterURL, mp4URL, webpURL} {
		if code := public(k); code != http.StatusNotFound {
			t.Fatalf("draft %s: %d", k, code)
		}
	}
	if code, _ := images(""); code != http.StatusNotFound {
		t.Fatalf("draft video-images: %d", code)
	}
	code, ed := images("editor")
	if code != http.StatusOK || len(ed.Poster.Outputs) != 1 || len(ed.HoverPreview.MP4) != 1 {
		t.Fatalf("editor video-images: %d %+v", code, ed)
	}
	for _, u := range []string{ed.Poster.Outputs[0].URL, ed.HoverPreview.MP4[0].URL, ed.HoverPreview.WebP[0].URL} {
		if !strings.Contains(u, "/editor/") || fetch(u) != http.StatusOK {
			t.Fatalf("editor url %s", u)
		}
	}

	// Paid: the poster is the teaser; no hover clip.
	publish(access.Resolution{Visible: true})
	if public(posterURL) != http.StatusOK || public(mp4URL) != http.StatusNotFound || public(webpURL) != http.StatusNotFound {
		t.Fatalf("paid: poster %d mp4 %d webp %d", public(posterURL), public(mp4URL), public(webpURL))
	}
	code, v := images("viewer")
	if code != http.StatusOK || len(v.Poster.Outputs) != 1 || !strings.HasPrefix(v.Poster.Outputs[0].URL, mediaSrv.URL+"/"+posterURL) ||
		len(v.HoverPreview.MP4) != 0 || v.HoverPreview.Pending {
		t.Fatalf("paid video-images: %d %+v", code, v)
	}
	if fetch(v.Poster.Outputs[0].URL) != http.StatusOK {
		t.Fatal("published poster url")
	}

	// Free: both.
	publish(access.Resolution{Visible: true, Accessible: true})
	for _, k := range []string{posterURL, mp4URL, webpURL} {
		if code := public(k); code != http.StatusOK {
			t.Fatalf("free %s: %d", k, code)
		}
	}
	if code, v := images(""); code != http.StatusOK || len(v.HoverPreview.WebP) != 1 || fetch(v.HoverPreview.WebP[0].URL) != http.StatusOK {
		t.Fatalf("free video-images: %d %+v", code, v)
	}

	// A host policy that allows no teaser for paid items.
	strict, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: kinds, Resolver: vis,
		Exposure: func(_ context.Context, _ contentref.ContentRef, res access.Resolution) media.Exposure {
			return media.Exposure{Poster: res.Full(), HoverPreview: res.Full()}
		}})
	if err != nil {
		t.Fatal(err)
	}
	vis.set(access.Resolution{Visible: true})
	if err := strict.Publish(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if public(posterURL) != http.StatusNotFound {
		t.Fatal("strict policy published the poster of a paid item")
	}

	// Unpublished (or deleted): gone at once.
	publish(access.Resolution{Visible: true, Accessible: true})
	publish(access.Resolution{})
	for _, k := range []string{posterURL, mp4URL, webpURL} {
		if code := public(k); code != http.StatusNotFound {
			t.Fatalf("unpublished %s: %d", k, code)
		}
	}
	if code, _ := images("viewer"); code != http.StatusNotFound {
		t.Fatalf("unpublished video-images: %d", code)
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
		Resolver: verdicts{"1": {Visible: true, Accessible: true}},
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
		if rec := get("scraper", "/post/1"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := get("scraper", "/post/1/hls/a/master.m3u8")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("over the limit: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := get("someone-else", "/post/1"); rec.Code != http.StatusOK {
		t.Fatalf("another viewer: %d", rec.Code)
	}
	for i := range 3 {
		if rec := get("", "/post/1"); rec.Code != http.StatusOK {
			t.Fatalf("anonymous %d: %d", i, rec.Code)
		}
	}
	if rec := get("", "/post/1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("anonymous from one address over the limit: %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if rec := get("scraper", "/post/1"); rec.Code != http.StatusOK {
		t.Fatalf("after refill: %d", rec.Code)
	}
}
