package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"slices"
	"sync"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
)

const slotBase = "https://media.example"

// paint fills a w×h image by fn(x, y).
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

// slot uploads body to a slot and commits it with edit.
func (e *env) slot(t *testing.T, ref contentref.ContentRef, slot string, body []byte, edit *media.Edit) {
	t.Helper()
	sum := sha256.Sum256(body)
	p, err := e.uploads.Presign(context.Background(), access.Actor{ID: "u"}, media.PresignRequest{Ref: ref, Type: http.DetectContentType(body),
		Size: int64(len(body)), SHA256: sum[:], Slot: slot})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
	for k := range p.Put.Header {
		req.Header.Set(k, p.Put.Header.Get(k))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if err := e.uploads.CommitSlot(context.Background(), access.Actor{ID: "u"}, ref, slot, sum[:], edit); err != nil {
		t.Fatal(err)
	}
}

func (e *env) editSlot(t *testing.T, ref contentref.ContentRef, slot string, edit *media.Edit) error {
	t.Helper()
	return e.uploads.EditSlot(context.Background(), access.Actor{ID: "u"}, ref, slot, edit)
}

func (e *env) slotManifest(t *testing.T, ref contentref.ContentRef, slot string) media.SlotManifest {
	t.Helper()
	m, err := e.manifests.SlotManifest(context.Background(), media.OutputURLs{BaseURL: slotBase + "/"}, ref, slot)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func widths(m media.SlotManifest) []int {
	var w []int
	for _, o := range m.Outputs {
		w = append(w, o.W)
	}
	return w
}

// checkOutputs requires m to be settled and list exactly want (at 3:1), each
// stored at its versioned URL with those dimensions and sampled pixels c.
func (e *env) checkOutputs(t *testing.T, ref contentref.ContentRef, m media.SlotManifest, want []int, c color.RGBA) {
	t.Helper()
	if m.Pending || m.Error != "" || m.Version == "" || !slices.Equal(widths(m), want) {
		t.Fatalf("manifest %+v, want widths %v", m, want)
	}
	prefix := e.Tenant + "/" + ref.ContentKind + "/" + ref.ContentID + "/public/"
	for _, o := range m.Outputs {
		key := prefix + o.Name + ".webp"
		if o.URL != slotBase+"/"+key+"?v="+m.Version || o.H != cover.Height(o.W) {
			t.Fatalf("output %+v", o)
		}
		b, obj := e.object(t, key)
		if obj.ContentType != "image/webp" || obj.CacheControl != "no-cache" || obj.Metadata["of"] != m.Version {
			t.Fatalf("%s: %+v", key, obj)
		}
		img, err := webp.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if s := img.Bounds().Size(); s.X != o.W || s.Y != o.H {
			t.Fatalf("%s is %v, want %dx%d", key, s, o.W, o.H)
		}
		for _, f := range [][2]float64{{0.1, 0.15}, {0.5, 0.5}, {0.9, 0.85}} {
			r, g, b, _ := img.At(int(f[0]*float64(o.W)), int(f[1]*float64(o.H))).RGBA()
			if d := max(diff(r>>8, c.R), diff(g>>8, c.G), diff(b>>8, c.B)); d > 40 {
				t.Fatalf("%s at %v is %d,%d,%d, want %v", key, f, r>>8, g>>8, b>>8, c)
			}
		}
	}
	e.checkStamp(t, ref, "cover", m)
	for _, w := range []int{100, 150, 300, 400, 600} {
		if !slices.Contains(want, w) {
			if _, err := e.Env.Store.Head(context.Background(), prefix+media.SlotOutput("cover", w)+".webp"); !errors.Is(err, media.ErrNotFound) {
				t.Fatalf("width %d not produced but stored: %v", w, err)
			}
		}
	}
}

type visible struct{}

func (visible) Resolve(context.Context, contentref.ContentRef, access.Actor) (access.Resolution, error) {
	return access.Resolution{Visible: true}, nil
}

// checkStamp requires the host's stored stamp to rebuild m's outputs without
// reads, and to have reached the host before the record showed it.
func (e *env) checkStamp(t *testing.T, ref contentref.ContentRef, slot string, m media.SlotManifest) {
	t.Helper()
	e.mu.Lock()
	stamp, late := e.stamps[ref.String()+"#"+slot], e.late[e.stamps[ref.String()+"#"+slot]]
	e.mu.Unlock()
	if stamp == "" || stamp != m.Stamp() {
		t.Fatalf("stamp %q, manifest's %q", stamp, m.Stamp())
	}
	if late {
		t.Fatalf("stamp %q reported after the slot record made it visible", stamp)
	}
	r, err := media.NewReader(media.ReaderOptions{Manifests: e.manifests, Kinds: e.kinds, Resolver: visible{},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: slotBase, SigningKey: token.Key{ID: "k", Secret: make([]byte, 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	outs, err := r.SlotOutputs(ref, slot, stamp)
	if err != nil || !slices.Equal(outs, m.Outputs) {
		t.Fatalf("SlotOutputs(%q) = %+v %v, want %+v", stamp, outs, err, m.Outputs)
	}
}

func TestSlotEditWidthsAndSpecChange(t *testing.T) {
	e := newEnv(t, galleryKind())
	ctx := context.Background()
	ref := contentref.New(e.Tenant, "gallery", "5")
	quads := paint(1800, 1200, func(x, y int) color.RGBA {
		return [2][2]color.RGBA{{red, blue}, {green, white}}[y/600][x/900]
	})

	// The top-right quadrant, 900×300 (the height follows the width): every width fits.
	e.slot(t, ref, "cover", encodePNG(t, quads), crop(900, 0, 900, 0))
	if m := e.slotManifest(t, ref, "cover"); !m.Pending || len(m.Outputs) != 0 {
		t.Fatalf("before processing: %+v", m)
	}
	e.drain(t)
	m := e.slotManifest(t, ref, "cover")
	e.checkOutputs(t, ref, m, []int{150, 300, 600}, blue)
	if m.Dims == nil || *m.Dims != (media.Dims{W: 1800, H: 1200}) || m.Aspect != 3 || *m.Edit.Crop != (media.Crop{X: 900, Y: 0, W: 900, H: 300}) {
		t.Fatalf("manifest %+v", m)
	}
	first := m.Version

	// Unchanged: no read, no rewrite (a whole-item job also covers slots).
	e.store.reads.Store(0)
	for _, j := range []media.ProcessJob{{Ref: ref, Slot: "cover"}, {Ref: ref}} {
		if err := e.proc.Process(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if e.store.reads.Load() != 0 {
		t.Fatalf("unchanged slot re-encoded (reads %d)", e.store.reads.Load())
	}

	// Re-edit the kept original to the bottom-left, 450 wide: 600 is never
	// upscaled, so it is deleted. Two jobs race. The version changes.
	orig, _ := e.Env.Store.Head(ctx, e.Tenant+"/gallery/5/originals/cover")
	if err := e.editSlot(t, ref, "cover", crop(0, 600, 450, 0)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, j := range append(e.queue.take(), media.ProcessJob{Ref: ref, Slot: "cover"}) {
		wg.Go(func() {
			if err := e.proc.Process(ctx, j); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	m = e.slotManifest(t, ref, "cover")
	e.checkOutputs(t, ref, m, []int{150, 300}, green)
	if again, _ := e.Env.Store.Head(ctx, e.Tenant+"/gallery/5/originals/cover"); again.ETag != orig.ETag || m.Version == first {
		t.Fatalf("edit replaced the original or kept the version %s", m.Version)
	}

	// With the size known, an edit outside it or under the smallest width is refused at once.
	for _, bad := range []*media.Edit{crop(1500, 0, 600, 0), crop(0, 0, 90, 0)} {
		if ue, ok := media.AsUploadError(e.editSlot(t, ref, "cover", bad)); !ok || ue.Code != media.CodeInvalid {
			t.Fatalf("edit %+v accepted", bad.Crop)
		}
	}

	// A spec change re-encodes from the original with the stored edit and
	// drops retired widths.
	k := galleryKind()
	k.Slots = map[string]media.Slot{"cover": {Aspect: 3, Widths: []int{100, 400}}}
	e.useKind(t, k)
	if m := e.slotManifest(t, ref, "cover"); !m.Pending {
		t.Fatal("spec change not pending")
	}
	if err := e.proc.Process(ctx, media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	m = e.slotManifest(t, ref, "cover")
	e.checkOutputs(t, ref, m, []int{100, 400}, green)
	if *m.Edit.Crop != (media.Crop{X: 0, Y: 600, W: 450, H: 150}) {
		t.Fatalf("edit lost: %+v", m.Edit.Crop)
	}
}

func TestSlotOrientationCentreAndFailure(t *testing.T) {
	e := newEnv(t, galleryKind())
	bands := func(y int) color.RGBA { return []color.RGBA{red, green, blue}[y/400] }

	// Portrait 400×1200, no edit: the centred 3:1 band (y 533..666) is
	// green; 400px wide, so 600 is skipped.
	portrait := contentref.New(e.Tenant, "gallery", "7")
	e.slot(t, portrait, "cover", encodePNG(t, paint(400, 1200, func(_, y int) color.RGBA { return bands(y) })), nil)
	e.drain(t)
	m := e.slotManifest(t, portrait, "cover")
	e.checkOutputs(t, portrait, m, []int{150, 300}, green)
	if m.Edit != nil || *m.Dims != (media.Dims{W: 400, H: 1200}) {
		t.Fatalf("manifest %+v", m)
	}

	// Stored 1200×400 with EXIF orientation 6 displays as the same 400×1200
	// portrait; the crop addresses the displayed image (its blue bottom band).
	rotated := contentref.New(e.Tenant, "gallery", "8")
	e.slot(t, rotated, "cover", orientedJPEG(t, paint(1200, 400, func(x, _ int) color.RGBA { return bands(x) }), 6), crop(0, 850, 300, 0))
	e.drain(t)
	m = e.slotManifest(t, rotated, "cover")
	e.checkOutputs(t, rotated, m, []int{150, 300}, blue)
	if *m.Dims != (media.Dims{W: 400, H: 1200}) || len(e.failed) != 0 {
		t.Fatalf("manifest %+v, failed %v", m, e.failed)
	}

	// A committed edit the original cannot take (checked by the job, as the
	// new original's size is unknown) fails once and keeps the served outputs.
	e.slot(t, rotated, "cover", orientedJPEG(t, paint(1200, 400, func(x, _ int) color.RGBA { return bands(x) }), 1), crop(0, 850, 300, 0))
	e.drain(t)
	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: rotated, Slot: "cover"}); err != nil {
		t.Fatal(err)
	}
	bad := e.slotManifest(t, rotated, "cover")
	if bad.Error == "" || bad.Pending || bad.Version != m.Version || !slices.Equal(widths(bad), []int{150, 300}) ||
		*bad.Dims != (media.Dims{W: 1200, H: 400}) || len(e.failed) != 1 {
		t.Fatalf("failed edit: %+v, failed %v", bad, e.failed)
	}

	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: portrait, Slot: "missing"}); err == nil {
		t.Fatal("unknown slot accepted")
	}
}
