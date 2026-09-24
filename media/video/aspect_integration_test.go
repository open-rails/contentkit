package video_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

// Rungs are short sides: labels and keys stay {N}p while w×h follows the
// source aspect, capped at 4096 per side and a 3840×2160 area.
func TestAspectLadders(t *testing.T) {
	e := newEnv(t, nil, nil)
	for _, c := range []struct {
		name         string
		src          fixture
		ladder       []int
		want         [][3]int // rung, w, h
		tileW, tileH int
		codec        string // top rung's CODECS prefix
	}{
		{"16:9", fixture{w: 320, h: 180, secs: 2}, []int{180, 120}, [][3]int{{180, 320, 180}, {120, 214, 120}}, 160, 90, "avc1.64"},
		{"21:9", fixture{w: 420, h: 180, secs: 2}, []int{180, 120}, [][3]int{{180, 420, 180}, {120, 280, 120}}, 210, 90, "avc1.64"},
		{"9:21", fixture{w: 180, h: 420, secs: 2}, []int{180, 120}, [][3]int{{180, 180, 420}, {120, 120, 280}}, 90, 210, "avc1.64"},
		// Over 4K: a 4800-wide 21:9 source is capped to 4096×1756 at level 5.1.
		{"over 4K", fixture{w: 4800, h: 2058, secs: 2, rate: 2}, []int{2000, 240}, [][3]int{{2000, 4096, 1756}, {240, 560, 240}}, 210, 90, "avc1.640033"},
	} {
		t.Run(c.name, func(t *testing.T) {
			op := media.OpReplace
			if m, _, _ := e.manifests.Get(context.Background(), e.ref); m == nil || m.File("source") < 0 {
				op = media.OpInsert
			}
			e.commit(t, c.src.make(t), op)
			job := video.Job{Ref: e.ref, Versioned: true, Video: media.Video{Ladder: c.ladder}}
			if err := e.encoder.Encode(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			m, _ := e.manifest(t)
			f := m.Files[m.File("source")]
			var got [][3]int
			for _, r := range f.HLS.Video {
				got = append(got, [3]int{r.Rung, r.Width, r.Height})
				p := ffprobe(t, e.blob(t, r.Blob))
				if s := p.Streams[0]; s.Width != r.Width || s.Height != r.Height {
					t.Fatalf("rendition %dp is %dx%d, manifest %dx%d", r.Rung, s.Width, s.Height, r.Width, r.Height)
				}
				d, ok := m.Downloads[video.DownloadKey("source", r.Rung)]
				if !ok {
					t.Fatalf("no download for %dp: %v", r.Rung, m.Downloads)
				}
				for _, s := range ffprobe(t, e.blob(t, d.Blob)).Streams {
					if s.CodecType == "video" && (s.Width != r.Width || s.Height != r.Height) {
						t.Fatalf("download %dp is %dx%d", r.Rung, s.Width, s.Height)
					}
				}
			}
			if !slices.Equal(got, c.want) || !strings.HasPrefix(f.HLS.Video[0].Codecs, c.codec) {
				t.Fatalf("ladder %v %s, want %v %s", got, f.HLS.Video[0].Codecs, c.want, c.codec)
			}
			sp := f.HLS.Sprite
			if sp.Width != c.tileW || sp.Height != c.tileH {
				t.Fatalf("sprite tile %dx%d, want %dx%d", sp.Width, sp.Height, c.tileW, c.tileH)
			}
			if s := ffprobe(t, e.blob(t, sp.Blob)).Streams[0]; s.Width != 10*c.tileW || s.Height != 10*c.tileH {
				t.Fatalf("sprite image %dx%d", s.Width, s.Height)
			}
			if w, h := f.Meta["w"], f.Meta["h"]; fmt.Sprint(w, h) != fmt.Sprint(c.src.w, c.src.h) {
				t.Fatalf("meta %v", f.Meta)
			}
		})
	}
}

// A source outside the kind's aspect bounds is recorded as failed, reported
// once through Hooks.Failed, and not retried until its source changes.
func TestAspectOutOfRangeFailsPermanently(t *testing.T) {
	e := newEnv(t, nil, nil)
	var failed atomic.Int32
	var reason error
	enc, err := video.New(video.Config{Store: e.store, TempDir: t.TempDir(), Threads: 2,
		Hooks: media.Hooks{Failed: func(_ context.Context, ref contentref.ContentRef, file string, err error) {
			if ref == e.ref && file == "source" {
				failed.Add(1)
				reason = err
			}
		}}})
	if err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 480, h: 180, secs: 1}.make(t), media.OpInsert) // 2.67:1, wider than 21:9
	for range 2 {
		if err := enc.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if failed.Load() != 1 || !errors.Is(reason, video.ErrAspect) || h == nil || len(h.Video) != 0 ||
		!strings.Contains(h.Error, "480x180") || len(m.Downloads) != 0 {
		t.Fatalf("failed %d (%v), hls %+v, downloads %v", failed.Load(), reason, h, m.Downloads)
	}

	// Widening the kind's bounds retries it.
	if err := enc.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true, Video: media.Video{MaxAspect: 3}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = e.manifest(t); m.Files[0].HLS.Error != "" || len(m.Files[0].HLS.Video) != 1 || m.Files[0].HLS.Video[0].Width != 480 {
		t.Fatalf("hls %+v", m.Files[0].HLS)
	}
}
