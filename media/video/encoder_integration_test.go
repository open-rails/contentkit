package video_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
)

// encodeWith encodes a 720p fixture with c and returns its renditions.
func encodeWith(t *testing.T, c video.Config) []media.Rendition {
	t.Helper()
	e := newEnv(t, nil, nil)
	c.Store, c.Locker, c.TempDir = e.store, s3test.Locker(t, e.store), t.TempDir()
	var err error
	if e.encoder, err = video.New(c); err != nil {
		t.Skip(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 9, rate: 30, audio: 1, tone: 440}.make(t), media.OpInsert)
	if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := e.manifest(t)
	h := m.Files[m.File("source")].HLS
	if h == nil || len(h.Video) != 2*len(c.Codecs) || len(h.Audio) != 1 {
		t.Fatalf("hls %+v", h)
	}
	for _, r := range h.Video {
		checkByteRanges(t, e.blob(t, r.Blob), r.Segments, "video", 9)
		if r.Height != r.Rung || len(r.Segments) != len(h.Video[0].Segments) {
			t.Fatalf("rendition %+v", r)
		}
	}
	return h.Video
}

var codecPrefix = map[media.Codec]string{media.CodecH264: "avc1.64", media.CodecHEVC: "hvc1.1.6.L", media.CodecAV1: "av01.0."}

// NVENC produces a valid ladder in every codec where a probe encode works;
// skipped elsewhere (CI).
func TestNVENCLadder(t *testing.T) {
	codecs := []media.Codec{media.CodecAV1, media.CodecHEVC, media.CodecH264}
	for _, r := range encodeWith(t, video.Config{Encoder: video.EncoderNVENC, Codecs: codecs}) {
		if !strings.HasPrefix(r.Codecs, codecPrefix[r.Codec]) {
			t.Fatalf("rendition %+v", r)
		}
	}
}

// AV1 (SVT-AV1 on the CPU) is an optional codec: av01 CODECS, on the other
// codecs' segments.
func TestAV1Ladder(t *testing.T) {
	vs := encodeWith(t, video.Config{Encoder: video.EncoderCPU, Threads: 2, Codecs: []media.Codec{media.CodecAV1, media.CodecH264}})
	if vs[0].Codec != media.CodecAV1 || vs[0].Rung != 720 || vs[2].Codec != media.CodecH264 {
		t.Fatalf("ladder %+v", vs)
	}
	for _, r := range vs {
		if !strings.HasPrefix(r.Codecs, codecPrefix[r.Codec]) {
			t.Fatalf("rendition %+v", r)
		}
		for j, s := range r.Segments {
			if s.Seconds != vs[0].Segments[j].Seconds {
				t.Fatalf("%dp %s segment %d: %gs vs %gs", r.Rung, r.Codec, j, s.Seconds, vs[0].Segments[j].Seconds)
			}
		}
	}
}

func TestUnknownEncoderOrCodec(t *testing.T) {
	requireFFmpeg(t)
	store := s3test.Open(t).Store
	for _, c := range []video.Config{
		{Store: store, Encoder: "x264"},
		{Store: store, Codecs: []media.Codec{"vp9"}},
		{Store: store, Codecs: []media.Codec{media.CodecH264, media.CodecH264}},
	} {
		if _, err := video.New(c); err == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
}

// A pass NVENC fails on is encoded again on the CPU in the same scratch
// directory, over the tracks the failed pass already wrote.
func TestNVENCFallbackToCPU(t *testing.T) {
	e := newEnv(t, nil, nil)
	defer video.FailNVENC(e.encoder, media.CodecH264)()
	e.commit(t, fixture{w: 854, h: 480, secs: 5, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
	e.encode(t)
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if h == nil || len(h.Video) != 2 || len(h.Audio) != 1 || len(h.Subs) != 1 || !strings.HasPrefix(h.Video[1].Codecs, "avc1.64") {
		t.Fatalf("hls %+v", h)
	}
}
