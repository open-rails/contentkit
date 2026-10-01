package video_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

func TestEncoderConfig(t *testing.T) {
	e := newEnv(t, opts{})
	for _, c := range []video.Config{
		{Manifests: e.ms, Encoder: "x264"},
		{Manifests: e.ms, Codecs: []media.Codec{"vp9"}},
		{Manifests: e.ms, Codecs: []media.Codec{media.CodecH264, media.CodecH264}},
		{Codecs: []media.Codec{media.CodecH264}},
	} {
		if _, err := video.New(t.Context(), c); err == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if enc, err := video.New(ctx, video.Config{Manifests: e.ms, TempDir: t.TempDir(), Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}}); enc != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("encoder: %v, error: %v", enc, err)
	}
}

// NVENC encodes a valid ladder in every codec where a probe encode works;
// skipped elsewhere (CI).
func TestNVENCLadder(t *testing.T) {
	e := newEnv(t, opts{encoder: video.EncoderNVENC, codecs: []media.Codec{media.CodecAV1, media.CodecHEVC, media.CodecH264}})
	e.start()
	e.put("source", "video/x-matroska", fixture{w: 1280, h: 720, secs: 5, rate: 30, audio: 1, tone: 440}.make(t), nil)
	e.wait()
	for _, o := range e.manifest().Outputs("source.mkv", "hls") {
		if o.Track.Kind == media.TrackVideo {
			checkByteRanges(t, e.blob(o.Blob), e.index(o).Segments, "video", 5)
		}
	}
}

// A pass NVENC fails on is encoded again on the CPU in the same scratch
// directory, over the tracks the failed pass already wrote.
func TestNVENCFallbackToCPU(t *testing.T) {
	e := newEnv(t, opts{})
	defer video.FailNVENC(e.enc, media.CodecH264)()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 854, h: 480, secs: 5, audio: 1, subs: true, tone: 440}.make(t), nil)
	e.wait()
	m := e.manifest()
	if got := outputPaths(m, "source.mkv", "hls"); len(got) != 4 || !strings.HasPrefix(e.file(m, "hls/480-h264.mp4").Track.Codecs, "avc1.64") {
		t.Fatalf("outputs %v", got)
	}
}
