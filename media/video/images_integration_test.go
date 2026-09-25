package video_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
)

const base = "https://media.example"

var quadrant = videotest.Quadrant

// cid is the n-th test content id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func (e *env) get(t *testing.T, key string) []byte {
	t.Helper()
	rc, _, err := e.store.Get(context.Background(), key, media.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keyOf(u string) string {
	k, _, _ := strings.Cut(strings.TrimPrefix(u, base+"/"), "?")
	return k
}

func (e *env) images(t *testing.T) media.VideoImages {
	t.Helper()
	v, err := e.manifests.VideoImages(context.Background(), media.OutputURLs{BaseURL: base, EditorToken: "tok"}, e.ref, true, "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) posterRecord(t *testing.T) *media.SlotRecord {
	t.Helper()
	rec, err := e.manifests.Slot(context.Background(), e.ref.Content(), media.PosterSlot)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// frame is the grabbed poster original: the committed PNG, handed to the image job.
func (e *env) frame(t *testing.T, w, h int) image.Image {
	t.Helper()
	rec := e.posterRecord(t)
	key := e.item(t).OriginalsPrefix() + media.PosterSlot
	body := e.get(t, key)
	obj, err := e.store.Head(context.Background(), key)
	if err != nil || obj.ETag != rec.Original || obj.ContentType != "image/png" {
		t.Fatalf("poster original %+v %v, record %+v", obj, err, rec)
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("frame is %v, want %dx%d", b, w, h)
	}
	if !slices.Contains(e.slotJobs, media.ProcessJob{Ref: e.ref.Content(), Slot: media.PosterSlot}) {
		t.Fatalf("frame not handed to the image job: %+v", e.slotJobs)
	}
	return img
}

func (e *env) setPoster(t *testing.T, r media.PosterRequest) {
	t.Helper()
	if err := e.uploads.SetVideoPoster(context.Background(), admin, e.ref, r); err != nil {
		t.Fatal(err)
	}
}

// noPreviewClips fails if anything was rendered besides the poster: the
// hover-clip pipeline is gone (players preview the HLS itself).
func (e *env) noPreviewClips(t *testing.T) {
	t.Helper()
	item := e.item(t)
	for _, prefix := range []string{item.OriginalsPrefix(), item.EditorPrefix(), item.PublicPrefix()} {
		for o, err := range e.store.List(context.Background(), prefix) {
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(o.Key, "hover_preview") || prefix == item.PublicPrefix() && strings.HasSuffix(o.Key, ".mp4") {
				t.Fatalf("preview clip rendered: %s", o.Key)
			}
		}
	}
}

func TestDefaultPosterFrameSkipsBlackIntro(t *testing.T) {
	e := newEnv(t, nil, nil)
	source := e.commit(t, videotest.Segments(t, 0), media.OpInsert)
	e.encode(t)

	rec := e.posterRecord(t)
	if f := rec.Frame; f == nil || !f.Auto || f.Time != 4.2 || f.File != "source" || f.Version != "v1" || f.Source != source || rec.Edit != nil {
		t.Fatalf("auto poster %+v", rec)
	}
	img := e.frame(t, 640, 360)
	if quadrant(img, 0) != "red" || quadrant(img, 3) != "cyan" {
		t.Fatalf("auto frame: %s / %s, want red (the first frame with detail) / cyan", quadrant(img, 0), quadrant(img, 3))
	}

	e.noPreviewClips(t)

	v := e.images(t)
	if s := v.Poster.Selection; s == nil || s.Source != media.PosterSourceAuto || s.Time == nil || *s.Time != 4.2 || !v.Poster.Pending ||
		v.Poster.Time == nil || *v.Poster.Time != 4.2 || v.Poster.File != "source" {
		t.Fatalf("poster %+v (the image job has not run)", v.Poster)
	}
	if v.Video == nil || v.Video.Duration < 11.9 || v.Video.W != 640 || v.Video.H != 360 || !v.Video.Encoded {
		t.Fatalf("video info %+v", v.Video)
	}

	// Nothing changed: the next job grabs nothing.
	before := e.posterRecord(t)
	e.encode(t)
	if e.posterRecord(t).Original != before.Original {
		t.Fatal("unchanged selection was redone")
	}
}

func TestFramePosterSelectionAndRegrab(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, videotest.Segments(t, 0), media.OpInsert)
	e.encode(t)

	edit := &media.Edit{Crop: &media.Crop{X: 160, Y: 90, W: 480, H: 270}}
	e.setPoster(t, media.PosterRequest{Source: media.PosterSourceFrame, Time: 9.5, Edit: edit})
	if rec := e.posterRecord(t); rec.Original != "" || rec.Frame.Auto || rec.Frame.Time != 9.5 || rec.Frame.Source != "" ||
		rec.Edit == nil || *rec.Edit.Crop != (media.Crop{X: 160, Y: 90, W: 480, H: 270}) {
		t.Fatalf("selection before the job: %+v", rec)
	}
	e.encode(t)
	img := e.frame(t, 640, 360)
	if quadrant(img, 0) != "yellow" || quadrant(img, 3) != "cyan" {
		t.Fatalf("frame at 9.5s: %s / %s", quadrant(img, 0), quadrant(img, 3))
	}
	if rec := e.posterRecord(t); rec.Edit == nil || rec.Frame.Source == "" {
		t.Fatalf("grabbed record %+v", rec)
	}

	// Refused: off-frame and too-narrow edits, times outside the video, unknown sources.
	for _, r := range []media.PosterRequest{
		{Source: media.PosterSourceFrame, Time: 1, Edit: &media.Edit{Crop: &media.Crop{X: 400, Y: 0, W: 480, H: 270}}},
		{Source: media.PosterSourceFrame, Time: 1, Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 200, H: 112}}},
		{Source: media.PosterSourceFrame, Time: 12.5},
		{Source: media.PosterSourceFrame, Time: -1},
		{Source: media.PosterSourceAuto, Edit: edit},
		{Source: "sprite"},
	} {
		err := e.uploads.SetVideoPoster(context.Background(), admin, e.ref, r)
		want := media.CodeInvalid
		if r.Edit != nil && r.Edit.Crop != nil && r.Edit.Crop.W == 200 {
			want = media.CodeImageTooSmall
		}
		if ue, ok := media.AsUploadError(err); !ok || ue.Code != want {
			t.Fatalf("%+v: %v", r, err)
		}
	}

	e.setPoster(t, media.PosterRequest{Source: media.PosterSourceFrame, Time: 7.5})
	e.encode(t)
	if img := e.frame(t, 640, 360); quadrant(img, 0) != "blue" || e.posterRecord(t).Edit != nil {
		t.Fatalf("re-grab at 7.5s: %s", quadrant(img, 0))
	}

	e.setPoster(t, media.PosterRequest{Source: media.PosterSourceAuto})
	e.encode(t)
	if img := e.frame(t, 640, 360); quadrant(img, 0) != "red" || !e.posterRecord(t).Frame.Auto {
		t.Fatalf("auto again: %s", quadrant(img, 0))
	}
}

func ptr[T any](v T) *T { return &v }

func TestRotatedSourceFrame(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, videotest.Segments(t, 90), media.OpInsert)
	e.encode(t)

	// Displayed 360×640, encoded upright at 270×480: the frame is upscaled to
	// 480 wide, and the cyan box is top right.
	img := e.frame(t, 480, 854)
	if quadrant(img, 1) != "cyan" || quadrant(img, 0) != "red" || quadrant(img, 3) != "red" {
		t.Fatalf("rotated frame quadrants %s %s %s %s", quadrant(img, 0), quadrant(img, 1), quadrant(img, 2), quadrant(img, 3))
	}
	if v := e.images(t); v.Video.W != 480 || v.Video.H != 854 {
		t.Fatalf("rotated video info %+v", v.Video)
	}
}

type authz map[string]bool

func (a authz) CanUpload(_ context.Context, actor access.Actor, _ contentref.ContentRef) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: a[actor.ID], Exempt: true}, nil
}

func TestVideoImageRoutesAndFrameEndpoint(t *testing.T) {
	e := newEnv(t, nil, nil)
	frames, err := video.NewFrames(e.store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: e.store, Kinds: e.kinds, Manifests: e.manifests,
		Authorizer: authz{"admin": true}, Frames: frames, FrameConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	vis := &verdict{}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: e.manifests, Kinds: e.kinds, Resolver: vis,
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: base,
			SigningKey: token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(media.UploadHandler(uploads, media.UploadHandlerOptions{Tenant: e.Tenant, Reader: reader,
		Actor: func(r *http.Request) (access.Actor, bool) {
			id := r.Header.Get("X-Actor")
			return access.Actor{ID: id, Kind: "user"}, id != ""
		}}))
	defer srv.Close()
	call := func(actor, method, path string, body any) (int, []byte, http.Header) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		if actor != "" {
			req.Header.Set("X-Actor", actor)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	ref := media.RefBody{Kind: "video", ID: cid(88), Version: "v1"}
	frame := func(actor string, q url.Values) (int, []byte, http.Header) {
		q.Set("kind", "video")
		q.Set("id", cid(88))
		q.Set("version", "v1")
		return call(actor, "GET", "/frame?"+q.Encode(), nil)
	}

	e.commit(t, videotest.Segments(t, 0), media.OpInsert)
	if code, b, _ := frame("admin", url.Values{"t": {"4"}}); code != http.StatusConflict {
		t.Fatalf("frame before encode: %d %s", code, b)
	}
	if code, b, _ := call("admin", "POST", "/video-poster", media.VideoPosterBody{Ref: ref, Source: "frame", Time: ptr(4.0)}); code != http.StatusConflict {
		t.Fatalf("poster before encode: %d %s", code, b)
	}
	e.encode(t)

	jpegAt := func(q url.Values) image.Image {
		t.Helper()
		code, b, h := frame("admin", q)
		if code != http.StatusOK || h.Get("Content-Type") != "image/jpeg" || !strings.Contains(h.Get("Cache-Control"), "private") {
			t.Fatalf("frame %v: %d %s", q, code, b)
		}
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	for _, c := range []struct {
		t, w   string
		width  int
		colour string
	}{
		{"6", "", 320, "lime"},          // default width
		{"7.5", "160", 160, "blue"},     // exact segment
		{"-5", "5", 64, "black"},        // clamped to 0 and the minimum width
		{"999", "5000", 640, "magenta"}, // clamped to the end and the widest rendition
	} {
		img := jpegAt(url.Values{"t": {c.t}, "w": {c.w}})
		if img.Bounds().Dx() != c.width || quadrant(img, 0) != c.colour {
			t.Fatalf("frame t=%s w=%s: %v %s", c.t, c.w, img.Bounds(), quadrant(img, 0))
		}
	}
	if code, _, _ := frame("", url.Values{"t": {"4"}}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous frame: %d", code)
	}
	if code, _, _ := frame("viewer", url.Values{"t": {"4"}}); code != http.StatusForbidden {
		t.Fatalf("viewer frame: %d", code)
	}
	if code, _, _ := frame("admin", url.Values{"t": {"x"}}); code != http.StatusBadRequest {
		t.Fatalf("bad t: %d", code)
	}

	code, b, _ := call("admin", "POST", "/video-poster", media.VideoPosterBody{Ref: ref, Source: "frame", Time: ptr(7.5),
		Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 480, H: 270}}})
	var v media.VideoImages
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Poster.Selection == nil || *v.Poster.Selection.Time != 7.5 ||
		!v.Poster.Pending || v.Poster.Edit == nil || v.Poster.Edit.Crop.H != 270 || v.Video == nil || v.Video.W != 640 {
		t.Fatalf("POST /video-poster: %d %s", code, b)
	}
	if code, _, _ := call("viewer", "POST", "/video-poster", media.VideoPosterBody{Ref: ref, Source: "frame", Time: ptr(7.5)}); code != http.StatusForbidden {
		t.Fatalf("viewer poster: %d", code)
	}
	if code, _, _ := call("admin", "POST", "/video-preview", map[string]any{"ref": ref}); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /video-preview still routed: %d", code)
	}
	e.encode(t)
	code, b, _ = call("admin", "POST", "/video-images", media.VideoImagesBody{Ref: ref})
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Video == nil || v.Poster.Time == nil || *v.Poster.Time != 7.5 {
		t.Fatalf("POST /video-images: %d %s", code, b)
	}
	if img := e.frame(t, 640, 360); quadrant(img, 0) != "blue" {
		t.Fatalf("frame after API selection: %s", quadrant(img, 0))
	}
	e.noPreviewClips(t)

	// The viewer read resolves: a draft is hidden; once published and free,
	// no selections, and the cover's file and time (inline previews start there).
	pub := httptest.NewServer(reader.Handler(media.HandlerOptions{Tenant: e.Tenant}))
	defer pub.Close()
	viewerImages := func() (int, []byte, http.Header) {
		t.Helper()
		resp, err := http.Get(pub.URL + "/video/" + cid(88) + "@v1/video-images")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	if code, b, _ := viewerImages(); code != http.StatusNotFound {
		t.Fatalf("draft video images: %d %s", code, b)
	}
	vis.set(access.Resolution{Visible: true, Accessible: true})
	jobs, err := media.NewJobs(media.JobsConfig{Store: e.store, Kinds: e.kinds, Resolver: vis})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Publish(context.Background(), e.ref); err != nil {
		t.Fatal(err)
	}
	code, b, hdr := viewerImages()
	var public media.VideoImages
	if code != http.StatusOK || hdr.Get("Cache-Control") != "private, no-store" || json.Unmarshal(b, &public) != nil ||
		public.Poster.Selection != nil || public.Video != nil || public.Poster.File != v.Video.File ||
		public.Poster.Time == nil || *public.Poster.Time != 7.5 {
		t.Fatalf("public video images: %d %s", code, b)
	}
}
