package video_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
)

// NVENC produces a valid ladder where a probe encode works; skipped elsewhere (CI).
func TestNVENCLadder(t *testing.T) {
	e := newEnv(t, nil, nil)
	var err error
	e.encoder, err = video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(), Encoder: video.EncoderNVENC})
	if err != nil {
		t.Skip(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 9, rate: 30, audio: 1, tone: 440}.make(t), media.OpInsert)
	if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := e.manifest(t)
	h := m.Files[m.File("source")].HLS
	if h == nil || len(h.Video) != 2 || len(h.Audio) != 1 {
		t.Fatalf("hls %+v", h)
	}
	for _, r := range h.Video {
		if !strings.HasPrefix(r.Codecs, "avc1.64") || r.Height != r.Rung {
			t.Fatalf("rendition %+v", r)
		}
		checkByteRanges(t, e.blob(t, r.Blob), r.Segments, "video", 9)
	}
}

func TestUnknownEncoder(t *testing.T) {
	requireFFmpeg(t)
	if _, err := video.New(video.Config{Store: s3test.Open(t).Store, Encoder: "vp9"}); err == nil {
		t.Fatal("unknown encoder accepted")
	}
}
