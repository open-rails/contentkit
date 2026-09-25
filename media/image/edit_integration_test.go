package image_test

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

var (
	red   = color.RGBA{255, 0, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	green = color.RGBA{0, 255, 0, 255}
	white = color.RGBA{255, 255, 255, 255}
)

// quadrants is a 400×200 PNG: red, blue on top; green, white below.
func quadrants(t *testing.T) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			img.Set(x, y, [2][2]color.RGBA{{red, blue}, {green, white}}[y/100][x/200])
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
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
		if d := max(diff(r>>8, want.R), diff(g>>8, want.G), diff(bl>>8, want.B)); d > 40 {
			t.Fatalf("pixel %v is %d,%d,%d, want %v", p, r>>8, g>>8, bl>>8, want)
		}
	}
}

func diff(a uint32, b uint8) uint32 {
	if a > uint32(b) {
		return a - uint32(b)
	}
	return uint32(b) - a
}

func crop(x, y, w, h int) *media.Edit { return &media.Edit{Crop: &media.Crop{X: x, Y: y, W: w, H: h}} }

func TestEditCropRotate(t *testing.T) {
	editor := media.Spec{Width: 200, Unedited: true, EditorOnly: true}
	k := galleryKind()
	k.Specs["editor"] = editor
	e := newEnv(t, k)
	ref := contentref.NewVersion(e.Tenant, "gallery", cid(21), "en")
	quad := quadrants(t)
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", quad)), ins("002.png", e.upload(t, ref, "", pngImage(t, 300, 300, 1))))
	e.drain(t)
	m0, _ := e.manifest(t, ref)
	if d := m0.Files[0].Dims; d == nil || *d != (media.Dims{W: 400, H: 200}) {
		t.Fatalf("dims %+v", d)
	}
	high := func(m *media.Manifest, i int) []byte {
		b, _ := e.blob(t, ref, m.Files[i].Variants["high"].Blob)
		return b
	}

	// Top half, rotated clockwise: red on top, blue below. Only 001 is re-derived,
	// and not its unedited variant.
	edit := &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 400, H: 100}, Rotate: 90}
	e.commit(t, ref, media.Op{Op: media.OpEdit, Name: "001.png", Edit: edit})
	e.store.reads.Store(0)
	e.drain(t)
	if n := e.store.reads.Load(); n != 1 {
		t.Fatalf("read %d originals, want 1", n)
	}
	m1, _ := e.manifest(t, ref)
	f := m1.Files[0]
	pixels(t, high(m1, 0), 100, 400, map[[2]int]color.RGBA{{50, 50}: red, {50, 350}: blue})
	for name, s := range k.Specs {
		v := f.Variants[name]
		switch {
		case name == "editor" && v != m0.Files[0].Variants[name]:
			t.Fatalf("unedited variant regenerated: %+v", v)
		case name != "editor" && (v.Spec != s.For(edit) || v.Blob == m0.Files[0].Variants[name].Blob):
			t.Fatalf("%s not regenerated: %+v", name, v)
		}
	}
	if !f.Variants["editor"].Editor || f.Variants["high"].Editor {
		t.Fatalf("editor flags %+v", f.Variants)
	}
	if f.Meta["w"] != float64(100) || f.Meta["h"] != float64(400) || *f.Dims != *m0.Files[0].Dims {
		t.Fatalf("meta %v dims %+v", f.Meta, f.Dims)
	}
	for name, v := range m1.Files[1].Variants {
		if v != m0.Files[1].Variants[name] {
			t.Fatalf("002 %s regenerated", name)
		}
	}
	if m1.Downloads["zip"].Inputs == m0.Downloads["zip"].Inputs {
		t.Fatal("zip not rebuilt")
	}
	zipOf(t, e, ref, m1)
	thumb, _ := e.blob(t, ref, f.Variants["thumb"].Blob)
	pixels(t, thumb, 100, 150, nil)

	// Another crop: the top-right quadrant.
	e.commit(t, ref, media.Op{Op: media.OpEdit, Name: "001.png", Edit: crop(200, 0, 200, 100)})
	e.drain(t)
	m2, _ := e.manifest(t, ref)
	pixels(t, high(m2, 0), 200, 100, map[[2]int]color.RGBA{{5, 5}: blue, {195, 95}: blue})

	// Undo: variants re-derive from the untouched original, identical to before.
	e.commit(t, ref, media.Op{Op: media.OpEdit, Name: "001.png"})
	e.drain(t)
	m3, _ := e.manifest(t, ref)
	if m3.Files[0].Edit != nil || m3.Files[0].Variants["high"] != m0.Files[0].Variants["high"] || m3.Files[0].Meta["w"] != float64(400) {
		t.Fatalf("undo: %+v", m3.Files[0])
	}
	if orig, _ := e.object(t, e.Tenant+"/gallery/"+cid(21)+"/originals/"+m3.Files[0].Original); !bytes.Equal(orig, quad) {
		t.Fatal("original modified")
	}
	e.store.reads.Store(0)
	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref}); err != nil || e.store.reads.Load() != 0 {
		t.Fatalf("settled item reprocessed: %v, %d reads", err, e.store.reads.Load())
	}

	// A crop set before the size was known and outside it fails at processing.
	e.commit(t, ref, media.Op{Op: media.OpInsert, Name: "003.png", Original: e.upload(t, ref, "", pngImage(t, 50, 50, 2)), Edit: crop(0, 0, 60, 60)})
	e.drain(t)
	m4, _ := e.manifest(t, ref)
	if len(e.failed) != 1 || e.failed[0] != "003.png" || len(m4.Files[2].Variants) != 0 {
		t.Fatalf("failed %v, variants %v", e.failed, m4.Files[2].Variants)
	}
}

func TestSlotFromFileCrop(t *testing.T) {
	k := galleryKind()
	k.Slots = map[string]media.Slot{"cover": {Aspect: media.Ratio("1:2"), Widths: []int{100}}}
	e := newEnv(t, k)
	ref := contentref.NewVersion(e.Tenant, "gallery", cid(22), "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", quadrants(t))))
	e.drain(t)
	cover := func() ([]byte, media.Object) {
		return e.object(t, e.slotOutput(t, ref, "cover", 100))
	}
	set := func(edit *media.Edit) {
		t.Helper()
		if err := e.uploads.SetSlotFromFile(context.Background(), access.Actor{ID: "u"}, media.SlotFromFile{Ref: ref, Slot: "cover", File: "001.png", Edit: edit}); err != nil {
			t.Fatal(err)
		}
		e.drain(t)
	}

	// The height follows the slot's 1:2 aspect: the right half's left column.
	set(crop(200, 0, 100, 0))
	b, obj := cover()
	pixels(t, b, 100, 200, map[[2]int]color.RGBA{{50, 50}: blue, {50, 150}: white})

	// Same page, another crop: the original's bytes are unchanged, the cover is not.
	set(crop(0, 0, 100, 0))
	b2, obj2 := cover()
	if obj2.ETag == obj.ETag {
		t.Fatal("cover kept its ETag")
	}
	pixels(t, b2, 100, 200, map[[2]int]color.RGBA{{50, 50}: red, {50, 150}: green})
	e.store.reads.Store(0)
	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref.Content(), Slot: "cover"}); err != nil || e.store.reads.Load() != 0 {
		t.Fatalf("unchanged slot re-encoded: %v", err)
	}

	// Rotated clockwise: the crop's height follows so the result is 1:2, the
	// crop's left (red) on top.
	set(&media.Edit{Crop: &media.Crop{X: 100, Y: 0, W: 200}, Rotate: 90})
	b3, _ := cover()
	pixels(t, b3, 100, 200, map[[2]int]color.RGBA{{50, 50}: red, {50, 150}: blue})

	// Out of the page's bounds.
	err := e.uploads.SetSlotFromFile(context.Background(), access.Actor{ID: "u"}, media.SlotFromFile{Ref: ref, Slot: "cover", File: "001.png", Edit: crop(0, 150, 50, 0)})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeInvalid {
		t.Fatalf("outside: %v", err)
	}
}

// CONTENTKIT_TEST_FFMPEG=1 (the CI media job) fails instead of skipping without ffmpeg.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
}

func TestMixedImagesAndVideo(t *testing.T) {
	requireFFmpeg(t)
	k := media.Kind{Name: "post", Types: []string{"image/png", "video/x-matroska"}, MaxBytes: 10 << 20, Video: &media.Video{},
		Specs: map[string]media.Spec{"thumb": thumb}, TypeLimits: map[string]media.Limit{"video": {MaxFiles: 1}}}
	e := newEnv(t, k)
	ref := contentref.New(e.Tenant, "post", cid(23))
	clip := filepath.Join(t.TempDir(), "clip.mkv")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "libx264", "-c:a", "aac", "-preset", "ultrafast", "-threads", "1", "-y", clip).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	body, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	e.commit(t, ref, ins("a.png", e.upload(t, ref, "", quadrants(t))), ins("clip.mkv", e.uploadAs(t, ref, "", "video/x-matroska", body)))
	enc, err := video.New(video.Config{Store: e.Env.Store, TempDir: t.TempDir(), Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		e.drain(t)
		if err := enc.Encode(context.Background(), video.Job{Ref: ref}, nil); err != nil {
			t.Fatal(err)
		}
		// The grabbed poster frame's render, which the video worker hands to the image job.
		if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref, Slot: media.PosterSlot}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	m, etag := e.manifest(t, ref)
	img, vid := m.Files[0], m.Files[1]
	if len(img.Variants) != 1 || img.HLS != nil || img.Dims == nil {
		t.Fatalf("image: %+v", img)
	}
	if len(vid.Variants) != 0 || vid.HLS == nil || len(vid.HLS.Video) == 0 || vid.Dims != nil || vid.Meta["duration"] == nil {
		t.Fatalf("video: %+v", vid)
	}
	if _, ok := m.Downloads[video.DownloadKey("clip.mkv", 120)]; !ok {
		t.Fatalf("downloads: %v", m.Downloads)
	}
	// Each processor leaves the other's work alone: a rerun changes nothing.
	e.queue.jobs = []media.ProcessJob{{Ref: ref}}
	run()
	if _, again := e.manifest(t, ref); again != etag {
		t.Fatal("rerun rewrote the manifest")
	}

	_, err = e.uploads.Commit(context.Background(), access.Actor{ID: "u"}, ref, []media.Op{{Op: media.OpEdit, Name: "clip.mkv", Edit: &media.Edit{Rotate: 90}}})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeInvalid {
		t.Fatalf("video edit: %v", err)
	}
	_, err = e.uploads.Commit(context.Background(), access.Actor{ID: "u"}, ref, []media.Op{ins("clip2.mkv", m.Files[1].Original)})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeTooManyFiles {
		t.Fatalf("second video: %v", err)
	}
}
