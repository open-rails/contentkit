package video_test

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// frame decodes the poster upload's grabbed PNG.
func (e *env) frame(f media.File) image.Image {
	e.t.Helper()
	img, err := png.Decode(bytes.NewReader(must(os.ReadFile(e.blob(f.Blob)))))
	if err != nil {
		e.t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != f.W || b.Dy() != f.H {
		e.t.Fatalf("frame %dx%d, recorded %dx%d", b.Dx(), b.Dy(), f.W, f.H)
	}
	return img
}

// jobs counts the schema's jobs of kind.
func (e *env) jobs(kind string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+e.schema+`.river_job WHERE kind = $1`, kind).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A frame upload is grabbed from its video at the video's display size: an
// automatic one skips the black intro (from the source, before the ladder
// exists); a chosen time after the encode comes from the widest rendition; a
// new video grabs again. Each grab hands the poster to the image job.
func TestFrames(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}})
	e.start()
	src := videotest.Segments(t, 0) // black until 3 s, then red, lime at 5 s, blue at 7 s
	m := e.put("source", "video/mp4", src, nil, media.Op{Op: media.OpFrame, Path: "poster", Auto: true})
	if p := e.file(m, "poster.png"); p.Blob != "" || !p.Frame.Auto {
		t.Fatalf("poster before the grab %+v", p)
	}
	e.wait()
	m = e.manifest()
	video1, poster := e.file(m, "source.mp4"), e.file(m, "poster.png")
	if poster.Blob == "" || poster.Type != "image/png" || poster.W != 640 || poster.H != 360 || !poster.Frame.Auto ||
		poster.Frame.Of != video1.Blob || poster.Frame.T < 3 || poster.Frame.T > 7 || len(poster.Pending) != 1 {
		t.Fatalf("auto poster %+v %+v", poster, poster.Frame)
	}
	if c := videotest.Dominant(e.frame(poster), e.frame(poster).Bounds()); c != "red" {
		t.Fatalf("auto poster at %gs is %s", poster.Frame.T, c)
	}
	// The commit queued a plan and an image job; the grab another of each
	// (the image job absorbed by the one still waiting).
	plans := (workqueue.VideoPlanArgs{}).Kind()
	if e.jobs((workqueue.ImageArgs{}).Kind()) != 1 || e.jobs(plans) != 2 {
		t.Fatalf("%d image jobs, %d plans", e.jobs((workqueue.ImageArgs{}).Kind()), e.jobs(plans))
	}

	// A chosen time, from the rendition.
	e.commit(media.Op{Op: media.OpFrame, Path: "poster", T: new(8.0)})
	e.wait()
	poster = e.file(e.manifest(), "poster.png")
	if poster.Frame.T != 8 || poster.Frame.Auto || poster.Frame.Of != video1.Blob || poster.W != 640 {
		t.Fatalf("poster at 8 s %+v %+v", poster, poster.Frame)
	}
	if c := videotest.Dominant(e.frame(poster), e.frame(poster).Bounds()); c != "blue" {
		t.Fatalf("poster at 8 s is %s", c)
	}
	if e.jobs(plans) != 4 {
		t.Fatalf("%d plans after the chosen frame", e.jobs(plans))
	}

	// A rotated video replaces the source: the poster is grabbed again, upright.
	e.put("source", "video/mp4", videotest.Segments(t, 90), nil)
	e.wait()
	m = e.manifest()
	video2, poster := e.file(m, "source.mp4"), e.file(m, "poster.png")
	if video2.W != 360 || video2.H != 640 || poster.Frame.Of != video2.Blob || poster.W != 360 || poster.H != 640 || poster.Frame.T != 8 {
		t.Fatalf("regrabbed poster %+v %+v of video %+v", poster, poster.Frame, video2)
	}
	img := e.frame(poster)
	if videotest.Quadrant(img, 1) != "cyan" || videotest.Quadrant(img, 2) != "blue" {
		t.Fatalf("rotated poster quadrants %s %s", videotest.Quadrant(img, 1), videotest.Quadrant(img, 2))
	}

	// The frame picker seeks the source.
	frames, err := video.NewFrames(e.store)
	if err != nil {
		t.Fatal(err)
	}
	b, err := frames.Frame(e.ctx, e.item(), video2, 6, 180)
	if err != nil {
		t.Fatal(err)
	}
	pick, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if pick.Bounds().Dx() != 180 || pick.Bounds().Dy() != 320 || videotest.Quadrant(pick, 2) != "lime" {
		t.Fatalf("picked frame %v, %s", pick.Bounds(), videotest.Quadrant(pick, 2))
	}
}
