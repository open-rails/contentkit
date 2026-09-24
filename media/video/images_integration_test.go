package video_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
)

const base = "https://media.example"

var quadrant = videotest.Quadrant

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
	v, err := e.manifests.VideoImages(context.Background(), base, e.ref, true, "")
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
	if n := len(e.slotJobs); n == 0 || e.slotJobs[n-1] != (media.ProcessJob{Ref: e.ref.Content(), Slot: media.PosterSlot}) {
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

func (e *env) setPreview(t *testing.T, r media.PreviewRequest) {
	t.Helper()
	if err := e.uploads.SetHoverPreview(context.Background(), admin, e.ref, r); err != nil {
		t.Fatal(err)
	}
}

// webpAnim parses an animated WebP: canvas, loop count, frame count and total
// milliseconds, plus the first frame decoded.
type webpAnim struct {
	w, h, loops, frames, ms int
	first                   image.Image
}

func parseWebPAnim(t *testing.T, b []byte) webpAnim {
	t.Helper()
	if len(b) < 16 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		t.Fatal("not a WebP")
	}
	if c := string(b[12:16]); c == "VP8 " || c == "VP8L" {
		// libwebp writes a still when every frame is identical.
		img, err := webp.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return webpAnim{w: img.Bounds().Dx(), h: img.Bounds().Dy(), frames: 1, ms: -1, first: img}
	}
	u24 := func(p []byte) int { return int(p[0]) | int(p[1])<<8 | int(p[2])<<16 }
	a := webpAnim{loops: -1}
	for p := b[12:]; len(p) >= 8; {
		id, n := string(p[:4]), int(binary.LittleEndian.Uint32(p[4:8]))
		body := p[8 : 8+n]
		switch id {
		case "VP8X":
			a.w, a.h = u24(body[4:])+1, u24(body[7:])+1
		case "ANIM":
			a.loops = int(binary.LittleEndian.Uint16(body[4:6]))
		case "ANMF":
			a.frames++
			a.ms += u24(body[12:])
			if a.first == nil {
				riff := append([]byte("RIFF\x00\x00\x00\x00WEBP"), body[16:]...)
				binary.LittleEndian.PutUint32(riff[4:], uint32(len(riff)-8))
				img, err := webp.Decode(bytes.NewReader(riff))
				if err != nil {
					t.Fatalf("first frame: %v", err)
				}
				a.first = img
			}
		}
		p = p[8+n+n%2:]
	}
	return a
}

// mp4Frame decodes the MP4's first or last frame.
func mp4Frame(t *testing.T, path string, last bool) image.Image {
	t.Helper()
	args := []string{"-v", "error", "-nostdin"}
	if last {
		args = append(args, "-sseof", "-0.2")
	}
	args = append(args, "-i", path, "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1")
	img, err := png.Decode(bytes.NewReader(videotest.Run(t, "ffmpeg", args...)))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// preview checks every hover-preview output: sizes, length, and the colours
// of its first and last frames.
func (e *env) preview(t *testing.T, length float64, widths []int, first, last string) {
	t.Helper()
	v := e.images(t).HoverPreview
	if v.Pending || v.Version == "" || len(v.MP4) != len(widths) || len(v.WebP) != len(widths) {
		t.Fatalf("hover preview %+v, want widths %v", v, widths)
	}
	frames := int(math.Round(length * 12))
	for i, w := range widths {
		h := media.VideoPoster.Height(w)
		if v.MP4[i].W != w || v.WebP[i].W != w || v.MP4[i].H != h || !strings.HasSuffix(v.MP4[i].URL, "?v="+v.Version) {
			t.Fatalf("outputs %+v %+v", v.MP4[i], v.WebP[i])
		}
		obj, err := e.store.Head(context.Background(), keyOf(v.MP4[i].URL))
		if err != nil || obj.ContentType != "video/mp4" || obj.Metadata["of"] != v.Version {
			t.Fatalf("mp4 object %+v %v", obj, err)
		}
		anim := parseWebPAnim(t, e.get(t, keyOf(v.WebP[i].URL)))
		// libwebp merges identical frames, so only the total time is fixed.
		if anim.w != w || anim.h != h || (anim.ms >= 0 && (anim.loops != 0 || math.Abs(float64(anim.ms)/1000-length) > 0.15)) {
			t.Fatalf("webp %d: %+v, want %dx%d looping over %.1fs", w, anim, w, h, length)
		}
		if got := videotest.Dominant(anim.first, image.Rect(0, 0, w/2, h/2)); got != first {
			t.Fatalf("webp %d first frame is %s, want %s", w, got, first)
		}
		path := filepath.Join(t.TempDir(), "p.mp4")
		if err := os.WriteFile(path, e.get(t, keyOf(v.MP4[i].URL)), 0o600); err != nil {
			t.Fatal(err)
		}
		p := ffprobe(t, path, "-count_packets")
		if p.count("audio") != 0 || p.count("video") != 1 || p.Streams[0].CodecName != "h264" || p.Streams[0].Width != w ||
			p.Streams[0].Height != h || math.Abs(p.duration(t)-length) > 0.15 {
			t.Fatalf("mp4 %d: %+v", w, p)
		}
		if n := p.Streams[0].NbPackets; n != fmt.Sprint(frames) && n != fmt.Sprint(frames+1) && n != fmt.Sprint(frames-1) {
			t.Fatalf("mp4 %d has %s frames, want %d", w, n, frames)
		}
		for j, want := range []string{first, last} {
			if got := videotest.Dominant(mp4Frame(t, path, j == 1), image.Rect(0, 0, w/2, h/2)); got != want {
				t.Fatalf("mp4 %d frame %d is %s, want %s", w, j, got, want)
			}
		}
	}
}

func TestDefaultPosterFrameSkipsBlackIntroAndDefaultPreview(t *testing.T) {
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

	// A quarter in (3 s) for 3 s: red, then lime from 5 s.
	pr, err := e.manifests.HoverPreview(context.Background(), e.ref)
	if err != nil || !pr.Auto || pr.Start != 3 || pr.Duration != 3 {
		t.Fatalf("auto preview %+v %v", pr, err)
	}
	e.preview(t, 3, []int{320, 640}, "red", "lime")

	v := e.images(t)
	if s := v.Poster.Selection; s == nil || s.Source != media.PosterSourceAuto || s.Time == nil || *s.Time != 4.2 || !v.Poster.Pending {
		t.Fatalf("poster %+v (the image job has not run)", v.Poster)
	}
	if v.Video == nil || v.Video.Duration < 11.9 || v.Video.W != 640 || v.Video.H != 360 || !v.Video.Encoded {
		t.Fatalf("video info %+v", v.Video)
	}

	// Nothing changed: the next job grabs and renders nothing.
	before, pv := e.posterRecord(t), e.images(t).HoverPreview.Version
	e.encode(t)
	if e.posterRecord(t).Original != before.Original || e.images(t).HoverPreview.Version != pv {
		t.Fatal("unchanged selections were redone")
	}
}

func TestFramePosterSelectionAndRegrab(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, videotest.Segments(t, 0), media.OpInsert)
	e.encode(t)

	edit := &media.Edit{Crop: &media.Crop{X: 160, Y: 90, W: 480}}
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
		{Source: media.PosterSourceFrame, Time: 1, Edit: &media.Edit{Crop: &media.Crop{X: 400, Y: 0, W: 480}}},
		{Source: media.PosterSourceFrame, Time: 1, Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 200}}},
		{Source: media.PosterSourceFrame, Time: 12.5},
		{Source: media.PosterSourceFrame, Time: -1},
		{Source: media.PosterSourceAuto, Edit: edit},
		{Source: "sprite"},
	} {
		err := e.uploads.SetVideoPoster(context.Background(), admin, e.ref, r)
		if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeInvalid {
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

func TestPreviewSectionAndRerender(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, videotest.Segments(t, 0), media.OpInsert)
	e.encode(t)

	start := 7.2
	e.setPreview(t, media.PreviewRequest{Start: &start, Duration: 1.5})
	if v := e.images(t).HoverPreview; !v.Pending || v.Selection == nil || v.Selection.Start != 7.2 || v.Selection.Duration != 1.5 || v.Selection.Auto {
		t.Fatalf("before the job: %+v", v)
	}
	e.encode(t)
	e.preview(t, 1.5, []int{320, 640}, "blue", "blue")

	start = 5 // default length: [5, 8)
	e.setPreview(t, media.PreviewRequest{Start: &start})
	e.encode(t)
	e.preview(t, 3, []int{320, 640}, "lime", "blue")

	for _, r := range []media.PreviewRequest{
		{Start: ptr(1.0), Duration: 7},
		{Start: ptr(1.0), Duration: 0.5},
		{Start: ptr(10.0), Duration: 3},
		{Start: ptr(-1.0)},
		{Duration: 2},
	} {
		err := e.uploads.SetHoverPreview(context.Background(), admin, e.ref, r)
		if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeInvalid {
			t.Fatalf("%+v: %v", r, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestRotatedSourceFrame(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, videotest.Segments(t, 90), media.OpInsert)
	e.encode(t)

	// Displayed 360×640, encoded upright at 270×480: the frame is upscaled so
	// its centred 16:9 crop is 480 wide, and the cyan box is top right.
	img := e.frame(t, 480, 854)
	if quadrant(img, 1) != "cyan" || quadrant(img, 0) != "red" || quadrant(img, 3) != "red" {
		t.Fatalf("rotated frame quadrants %s %s %s %s", quadrant(img, 0), quadrant(img, 1), quadrant(img, 2), quadrant(img, 3))
	}
	if v := e.images(t); v.Video.W != 480 || v.Video.H != 854 {
		t.Fatalf("rotated video info %+v", v.Video)
	}
	// The preview's centred 16:9 is 270 wide: only the 320 output.
	e.preview(t, 3, []int{320}, "red", "lime")
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
	srv := httptest.NewServer(media.UploadHandler(uploads, media.UploadHandlerOptions{Tenant: e.Tenant, PublicBaseURL: base,
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
	ref := media.RefBody{Kind: "video", ID: "88", Version: "v1"}
	frame := func(actor string, q url.Values) (int, []byte, http.Header) {
		q.Set("kind", "video")
		q.Set("id", "88")
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
		Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 480}}})
	var v media.VideoImages
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.Poster.Selection == nil || *v.Poster.Selection.Time != 7.5 ||
		!v.Poster.Pending || v.Poster.Edit == nil || v.Poster.Edit.Crop.H != 270 || v.Video == nil || v.Video.W != 640 {
		t.Fatalf("POST /video-poster: %d %s", code, b)
	}
	if code, _, _ := call("viewer", "POST", "/video-poster", media.VideoPosterBody{Ref: ref, Source: "frame", Time: ptr(7.5)}); code != http.StatusForbidden {
		t.Fatalf("viewer poster: %d", code)
	}
	code, b, _ = call("admin", "POST", "/video-preview", media.VideoPreviewBody{Ref: ref, Start: ptr(5.0), Duration: 2})
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.HoverPreview.Selection.Start != 5 || !v.HoverPreview.Pending {
		t.Fatalf("POST /video-preview: %d %s", code, b)
	}
	if code, b, _ := call("admin", "POST", "/video-preview", media.VideoPreviewBody{Ref: ref, Start: ptr(11.0), Duration: 2}); code != http.StatusBadRequest {
		t.Fatalf("preview past the end: %d %s", code, b)
	}
	e.encode(t)
	code, b, _ = call("admin", "POST", "/video-images", media.VideoImagesBody{Ref: ref})
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil || v.HoverPreview.Pending || len(v.HoverPreview.MP4) != 2 || v.Video == nil ||
		v.HoverPreview.MP4[0].URL != base+"/"+e.Tenant+"/video/88/public/hover_preview_320.mp4?v="+v.HoverPreview.Version {
		t.Fatalf("POST /video-images: %d %s", code, b)
	}
	if img := e.frame(t, 640, 360); quadrant(img, 0) != "blue" {
		t.Fatalf("frame after API selection: %s", quadrant(img, 0))
	}
	e.preview(t, 2, []int{320, 640}, "lime", "lime")

	// The public read: no selections, and no Resolve.
	reader, err := media.NewReader(media.ReaderOptions{Manifests: e.manifests, Kinds: e.kinds, Resolver: noResolve{t},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: base,
			SigningKey: token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}}})
	if err != nil {
		t.Fatal(err)
	}
	pub := httptest.NewServer(reader.Handler(media.HandlerOptions{Tenant: e.Tenant}))
	defer pub.Close()
	resp, err := http.Get(pub.URL + "/video/88@v1/video-images")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var public media.VideoImages
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-cache" || json.Unmarshal(b, &public) != nil ||
		public.Poster.Selection != nil || public.HoverPreview.Selection != nil || public.Video != nil || len(public.HoverPreview.WebP) != 2 {
		t.Fatalf("public video images: %d %s", resp.StatusCode, b)
	}
	mp4, webpURL, err := reader.HoverPreviewURLs(e.ref, public.HoverPreview.Version)
	if err != nil || mp4 != public.HoverPreview.MP4[0].URL || webpURL != public.HoverPreview.WebP[0].URL {
		t.Fatalf("static preview urls %s %s %v", mp4, webpURL, err)
	}
}

type noResolve struct{ t *testing.T }

func (r noResolve) Resolve(context.Context, contentref.ContentRef, access.Actor) (access.Resolution, error) {
	r.t.Error("public video images resolved the item")
	return access.Resolution{}, nil
}
