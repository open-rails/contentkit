package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	stdimage "image"
	"image/color"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/video"
)

// editorURLs lists posters from editor/, where the image job writes them.
var editorURLs = media.OutputURLs{BaseURL: slotBase, EditorToken: "tok"}

// The poster end to end: the video worker grabs frames into the poster slot
// and hands them to this image job, which encodes them (and uploads) through
// the slot's edit.
func TestVideoPosterFramesAndUploads(t *testing.T) {
	videotest.RequireFFmpeg(t)
	ctx := context.Background()
	u := access.Actor{ID: "u"}
	e := newEnv(t, media.Kind{Name: "video", Versioned: true, Video: &media.Video{PosterWidths: []int{480, 960, 1920}}, Types: []string{"video/mp4", "image/jpeg"}})
	ref := contentref.NewVersion(e.Tenant, "video", "7", "v1")
	enc, err := video.New(video.Config{Store: e.Env.Store, TempDir: t.TempDir(), Threads: 2, Slots: e.queue})
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(videotest.Segments(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	e.commit(t, ref, media.Op{Op: media.OpInsert, Name: "source", Original: e.uploadAs(t, ref, "", "video/mp4", src)})
	e.queue.take() // the image job has nothing to do for a video file
	encode := func() {
		t.Helper()
		if err := enc.Encode(ctx, video.Job{Ref: ref, Versioned: true, Video: media.Video{PosterWidths: []int{480, 960, 1920}}}, nil); err != nil {
			t.Fatal(err)
		}
		e.drain(t)
	}
	poster := func(widths ...int) []stdimage.Image {
		t.Helper()
		v, err := e.manifests.VideoImages(ctx, editorURLs, ref, true, "")
		if err != nil {
			t.Fatal(err)
		}
		m := v.Poster
		var got []int
		var out []stdimage.Image
		for _, o := range m.Outputs {
			got = append(got, o.W)
			key, _, _ := strings.Cut(strings.TrimPrefix(o.URL, slotBase+"/"), "?")
			b, _ := e.object(t, key)
			img, err := webp.Decode(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != o.W || img.Bounds().Dy() != o.H {
				t.Fatalf("%s is %v", o.URL, img.Bounds())
			}
			out = append(out, img)
		}
		if m.Pending || m.Error != "" || !slices.Equal(got, widths) {
			t.Fatalf("poster %+v, want widths %v", m, widths)
		}
		return out
	}
	q := videotest.Quadrant

	// Default: the first frame with detail (4.2 s, red), 640 wide, so only 480,
	// uncropped at the video's 16:9.
	encode()
	if v, _ := e.manifests.VideoImages(ctx, editorURLs, ref, true, ""); len(v.Poster.Outputs) != 1 || v.Poster.Outputs[0].H != 270 || math.Abs(v.Poster.Aspect-16.0/9) > 0.01 {
		t.Fatalf("native auto poster %+v", v.Poster)
	}
	if img := poster(480)[0]; q(img, 0) != "red" || q(img, 3) != "cyan" {
		t.Fatalf("auto poster %s / %s", q(img, 0), q(img, 3))
	}

	// A frame at 9.5 s, rotated half a turn by its edit.
	if err := e.uploads.SetVideoPoster(ctx, u, ref, media.PosterRequest{Source: media.PosterSourceFrame, Time: 9.5,
		Edit: &media.Edit{Rotate: 180}}); err != nil {
		t.Fatal(err)
	}
	e.drain(t) // the image job skips the slot until the frame is grabbed
	if v, _ := e.manifests.VideoImages(ctx, editorURLs, ref, true, ""); !v.Poster.Pending {
		t.Fatal("frame selection not pending")
	}
	encode()
	if img := poster(480)[0]; q(img, 0) != "cyan" || q(img, 3) != "yellow" {
		t.Fatalf("rotated frame poster %s / %s", q(img, 0), q(img, 3))
	}

	// Re-edit the grabbed frame without a new grab: its right three quarters.
	if err := e.uploads.EditSlot(ctx, u, ref, media.PosterSlot, &media.Edit{Crop: &media.Crop{X: 160, Y: 90, W: 480, H: 270}}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	img := poster(480)[0]
	if q(img, 2) != "yellow" || q(img, 3) != "cyan" {
		t.Fatalf("re-edited frame poster %s / %s", q(img, 2), q(img, 3))
	}
	before, _ := e.manifests.Slot(ctx, ref.Content(), media.PosterSlot)
	encode()
	if after, _ := e.manifests.Slot(ctx, ref.Content(), media.PosterSlot); after.Original != before.Original || after.Frame == nil {
		t.Fatalf("an edit re-grabbed the frame: %+v", after)
	}

	// An upload, EXIF-rotated: stored 1080×1920 red over blue, shown 1920×1080
	// blue | red. Its left half is all blue.
	photo := orientedJPEG(t, paint(1080, 1920, func(_, y int) color.RGBA {
		if y < 960 {
			return red
		}
		return blue
	}), 6)
	e.uploadAs(t, ref.Content(), media.PosterSlot, "image/jpeg", photo)
	sum := sha256.Sum256(photo)
	if err := e.uploads.SetVideoPoster(ctx, u, ref, media.PosterRequest{Source: media.PosterSourceUpload, SHA256: sum[:],
		Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 960, H: 1080}}}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	for i, img := range poster(480, 960) {
		for k := range 4 {
			if q(img, k) != "blue" {
				t.Fatalf("upload poster %d quadrant %d is %s", i, k, q(img, k))
			}
		}
	}
	v, _ := e.manifests.VideoImages(ctx, editorURLs, ref, true, "")
	if v.Poster.Selection == nil || v.Poster.Selection.Source != media.PosterSourceUpload || v.Poster.Dims == nil || *v.Poster.Dims != (media.Dims{W: 1920, H: 1080}) {
		t.Fatalf("upload poster %+v", v.Poster)
	}
	encode() // the worker leaves an uploaded poster alone
	if v2, _ := e.manifests.VideoImages(ctx, editorURLs, ref, true, ""); v2.Poster.Version != v.Poster.Version {
		t.Fatal("the worker replaced an uploaded poster")
	}

	// Back to automatic.
	if err := e.uploads.SetVideoPoster(ctx, u, ref, media.PosterRequest{Source: media.PosterSourceAuto}); err != nil {
		t.Fatal(err)
	}
	encode()
	if img := poster(480)[0]; q(img, 0) != "red" {
		t.Fatalf("auto again %s", q(img, 0))
	}
}
