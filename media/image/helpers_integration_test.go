package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// cid is the n-th test item id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

var (
	red   = color.RGBA{255, 0, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	green = color.RGBA{0, 255, 0, 255}
	white = color.RGBA{255, 255, 255, 255}
	// Frame i of animatedGIF is split down the middle: left[i] | right[i].
	left  = []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 0, 255}}
	right = []color.RGBA{{0, 255, 255, 255}, {255, 0, 255, 255}, {255, 255, 255, 255}, {0, 0, 0, 255}}
)

var stills = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// config is the registry the tests run: pages with private presets and a
// zip, a cropped public cover, animated avatars refused.
func config(ns string) media.Config {
	return media.Config{Namespace: ns, BaseURL: "https://media.test",
		Kinds: []media.Kind{
			{Name: "gallery", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "originals/{name}", Types: stills, MaxBytes: 10 << 20}, {Path: "cover", Types: stills, MaxBytes: 10 << 20}},
				Private: []media.Private{
					{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 100, Height: 150, Fit: media.FitCover}},
					{Name: "high", From: "originals/{name}", To: "high/{name}.webp", Image: &media.Image{Quality: 90}},
					{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "high/"},
				},
				Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{150, 300, 600},
					Image: media.Image{Aspect: media.Ratio("3:1"), MinWidth: 60}, Default: "cover.png"}},
			},
			{Name: "user", Uploads: []media.Upload{{Path: "avatar", Types: stills, MaxBytes: 10 << 20}},
				Public: []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{20, 40},
					Image: media.Image{Aspect: media.Square, Animation: media.AnimationReject}}}},
			{Name: "anim", Uploads: []media.Upload{{Path: "files/{name}", Types: stills, MaxBytes: 10 << 20}},
				Private: []media.Private{{Name: "large", From: "files/{name}", To: "large/{name}.webp", Image: &media.Image{Width: 40, Quality: 90}}}},
			{Name: "post", Uploads: []media.Upload{{Path: "files/{name}", Types: stills, MaxBytes: 10 << 20}},
				Private: []media.Private{{Name: "web", From: "files/{name}", To: "web/{name}.webp", Image: &media.Image{Width: 50, Height: 50}}}},
		}}
}

type allow struct{}

func (allow) CanUpload(context.Context, access.Actor, media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true}, nil
}

type visible struct{}

func (visible) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		out[r.Key()] = access.Resolution{Visible: true, Accessible: true}
	}
	return out, nil
}

type nopQueue struct{}

func (nopQueue) Enqueue(context.Context, media.ProcessJob) error { return nil }

// env is one test's image producer over real MinIO, with the app's uploads
// and manifests, recording failures and purges.
type env struct {
	*s3test.Env
	reg     *media.Registry
	ms      *media.Manifests
	up      *media.Uploads
	proc    *image.Processor
	mu      sync.Mutex
	failed  []string
	purged  []string
	editor  access.Actor
	mutated func(*media.Config)
}

func newEnv(t *testing.T, mutate func(*media.Config)) *env {
	t.Helper()
	e := &env{Env: s3test.Open(t), editor: access.Actor{ID: "editor"}, mutated: mutate}
	e.deploy(t, mutate)
	return e
}

// deploy (re)builds the registry, uploads and processor: a deploy with new
// presets.
func (e *env) deploy(t *testing.T, mutate func(*media.Config)) {
	t.Helper()
	cfg := config(e.Tenant)
	cfg.Hooks = media.Hooks{CanUpload: allow{}, Resolver: visible{}, Failed: func(_ context.Context, _ contentref.ContentRef, path string, _ error) {
		e.mu.Lock()
		e.failed = append(e.failed, path)
		e.mu.Unlock()
	}}
	if mutate != nil {
		mutate(&cfg)
	}
	var err error
	if e.reg, err = media.NewRegistry(cfg); err != nil {
		t.Fatal(err)
	}
	e.ms = s3test.Manifests(t, e.Store, e.reg, media.ManifestOptions{Journal: e.Journal()})
	if e.up, err = media.NewUploads(media.UploadOptions{Store: e.Store, Manifests: e.ms, Queue: nopQueue{}}); err != nil {
		t.Fatal(err)
	}
	if e.proc, err = image.New(image.Config{Store: e.Store, Manifests: e.ms, Purge: func(_ context.Context, keys []string) error {
		e.mu.Lock()
		e.purged = append(e.purged, keys...)
		e.mu.Unlock()
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) ref(t *testing.T, kind string, n int) contentref.ContentRef {
	t.Helper()
	ref, err := e.reg.Ref(kind, cid(n))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// put uploads body like the browser and commits it at path with extra ops.
func (e *env) put(t *testing.T, ref contentref.ContentRef, path, typ string, body []byte, extra ...media.Op) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(body)
	p, err := e.up.Presign(ctx, e.editor, media.PresignRequest{Ref: ref, Path: path, Type: typ, Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	if p.Put != nil {
		req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		req.Header = p.Put.Header.Clone()
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d", path, resp.StatusCode)
		}
	}
	e.commit(t, ref, append([]media.Op{{Op: media.OpPut, Path: p.Path, Blob: p.Blob}}, extra...)...)
}

// commit commits ops and places staged uploads, as the worker's place job does.
func (e *env) commit(t *testing.T, ref contentref.ContentRef, ops ...media.Op) {
	t.Helper()
	if _, err := e.up.Commit(context.Background(), e.editor, ref, ops); err != nil {
		t.Fatalf("commit %+v: %v", ops, err)
	}
	if _, err := e.ms.Place(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}

func (e *env) process(t *testing.T, job media.ProcessJob) {
	t.Helper()
	if err := e.proc.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func (e *env) manifest(t *testing.T, ref contentref.ContentRef) *media.Manifest {
	t.Helper()
	m, _, err := e.ms.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) file(t *testing.T, ref contentref.ContentRef, path string) media.File {
	t.Helper()
	f, ok := e.manifest(t, ref).Get(path)
	if !ok {
		t.Fatalf("no file %s", path)
	}
	return f
}

// read reads an object's bytes and metadata.
func (e *env) read(t *testing.T, key string) ([]byte, media.Object) {
	t.Helper()
	rc, obj, err := e.Store.Get(context.Background(), key, media.GetOptions{})
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b, obj
}

func (e *env) blob(t *testing.T, ref contentref.ContentRef, f media.File) []byte {
	t.Helper()
	item, _ := e.reg.Item(ref)
	key, _ := item.Blob(f.Blob)
	b, _ := e.read(t, key)
	return b
}

func (e *env) public(t *testing.T, ref contentref.ContentRef, name string) ([]byte, media.Object, bool) {
	t.Helper()
	item, _ := e.reg.Item(ref)
	for _, f := range e.manifest(t, ref).Files {
		for _, pub := range f.Public {
			if i := slices.Index(pub.Names, name); i >= 0 && pub.Ready() {
				name = pub.NamesOnDisk()[i]
			}
		}
	}
	key, _ := item.Public(name)
	rc, obj, err := e.Store.Get(context.Background(), key, media.GetOptions{})
	if errors.Is(err, media.ErrNotFound) {
		return nil, obj, false
	} else if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b, obj, true
}

func (e *env) takePurged() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.purged
	e.purged = nil
	return p
}

func paint(w, h int, fn func(x, y int) color.RGBA) *stdimage.RGBA {
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, fn(x, y))
		}
	}
	return img
}

func encodePNG(t *testing.T, img stdimage.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func solid(t *testing.T, w, h int, c color.RGBA) []byte {
	return encodePNG(t, paint(w, h, func(int, int) color.RGBA { return c }))
}

// quadrants is a 400×200 PNG: red, blue on top; green, white below.
func quadrants(t *testing.T) []byte {
	return encodePNG(t, paint(400, 200, func(x, y int) color.RGBA { return [2][2]color.RGBA{{red, blue}, {green, white}}[y/100][x/200] }))
}

// orientedJPEG encodes img with an EXIF Orientation tag.
func orientedJPEG(t *testing.T, img stdimage.Image, orientation byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, orientation, 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	j := b.Bytes()
	out := append([]byte{}, j[:2]...)
	out = append(out, 0xFF, 0xE1, byte((len(payload)+2)>>8), byte(len(payload)+2))
	out = append(out, payload...)
	return append(out, j[2:]...)
}

// pixels decodes a WebP and checks its size and the colour at each point.
func pixels(t *testing.T, b []byte, w, h int, at map[[2]int]color.RGBA) {
	t.Helper()
	img, err := webp.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != w || got.Y != h {
		t.Fatalf("size %v, want %dx%d", got, w, h)
	}
	for p, want := range at {
		r, g, bl, _ := img.At(p[0], p[1]).RGBA()
		if !near(color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255}, want) {
			t.Fatalf("pixel %v is %d,%d,%d, want %v", p, r>>8, g>>8, bl>>8, want)
		}
	}
}

func near(a, b color.RGBA) bool {
	d := func(x, y uint8) int { return max(int(x)-int(y), int(y)-int(x)) }
	return d(a.R, b.R) < 40 && d(a.G, b.G) < 40 && d(a.B, b.B) < 40
}

func webpSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	c, err := webp.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a webp: %v", err)
	}
	return c.Width, c.Height
}

// animatedGIF is 4 frames of 80×80, 100-400 ms each, looping 3 times.
func animatedGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{}
	for i := range left {
		pal = append(pal, left[i], right[i])
	}
	g := &gif.GIF{LoopCount: 3}
	for i := range left {
		f := stdimage.NewPaletted(stdimage.Rect(0, 0, 80, 80), pal)
		for y := range 80 {
			for x := range 80 {
				c := left[i]
				if x >= 40 {
					c = right[i]
				}
				f.Set(x, y, c)
			}
		}
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 10*(i+1))
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// frames decodes every frame of an image: per-frame size, count and delays.
func frames(t *testing.T, b []byte) (w, h, n int, delays []int) {
	t.Helper()
	p := vips.NewImportParams()
	p.NumPages.Set(-1)
	img, err := vips.LoadImageFromBuffer(b, p)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	delays, _ = img.PageDelay()
	return img.Width(), img.PageHeight(), img.Height() / img.PageHeight(), delays
}
