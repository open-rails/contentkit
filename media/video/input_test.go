package video

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestToolDiagnosticsRedactSignedURL(t *testing.T) {
	stderr := []byte("https://s3.local/bucket/video.mp4?X-Amz-Credential=private-id&X-Amz-Signature=private-signature: forbidden")
	redacted := redactToolURLs(stderr)
	if bytes.Contains(redacted, []byte("private-id")) || bytes.Contains(redacted, []byte("private-signature")) ||
		!bytes.Contains(redacted, []byte("[redacted URL]")) {
		t.Fatalf("signed URL appeared in tool diagnostics: %s", redacted)
	}
}

// A concat list (or any non-container input) must never be demuxed: it would
// let an upload make ffmpeg read other local files.
func TestInputsAreConfinedToContainerDemuxers(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mkv")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=5:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-y", clip).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	if _, err := probe(ctx, clip); err != nil {
		t.Fatalf("a matroska source must probe: %v", err)
	}
	list := filepath.Join(dir, "source")
	if err := os.WriteFile(list, []byte("ffconcat version 1.0\nfile clip.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := probe(ctx, list); err == nil {
		t.Fatalf("concat list probed: %+v", p)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	pl := plan{duration: 1, video: 0, width: 64, height: 48, rungs: []rung{{n: 48, w: 64, h: 48}}, tileW: 120, tileH: 90}
	if err := ladder(ctx, list, out, pl, pass{rung: pl.rungs[0], codecs: []media.Codec{media.CodecH264}, sprite: true,
		enc: encoding{encoders: map[media.Codec]string{media.CodecH264: "libx264"}, threads: 1, preset: "fast"}}, nil); err == nil {
		t.Fatal("concat list encoded")
	}
	var observed EncodeObservation
	if err := ladder(ctx, clip, out, pl, pass{rung: pl.rungs[0], codecs: []media.Codec{media.CodecH264},
		enc:     encoding{encoders: map[media.Codec]string{media.CodecH264: "libx264"}, threads: 1, preset: "fast"},
		observe: func(o EncodeObservation) { observed = o }}, nil); err != nil {
		t.Fatal(err)
	}
	if !observed.Succeeded || observed.SourceClass != "sd" || observed.OutputSeconds != pl.duration || observed.Duration <= 0 || observed.CPU <= 0 {
		t.Fatalf("encode observation: %+v", observed)
	}
}

func TestEncodeObservationUsesVideoDuration(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	clip := filepath.Join(dir, "audio-outlasts-video.mkv")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=5:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-y", clip).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	probed, err := probe(ctx, clip)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := newPlan(probed, &media.Video{Ladder: []int{48}})
	if err != nil {
		t.Fatal(err)
	}
	containerDuration, _ := strconv.ParseFloat(probed.Format.Duration, 64)
	if containerDuration < 2.5 || math.Abs(pl.duration-1) > 0.2 || len(pl.audio) != 1 {
		t.Fatalf("video duration %.2f, container duration %.2f: %+v", pl.duration, containerDuration, pl)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	var observed EncodeObservation
	if err := ladder(ctx, clip, out, pl, pass{rung: pl.rungs[0], codecs: []media.Codec{media.CodecH264},
		enc:     encoding{encoders: map[media.Codec]string{media.CodecH264: "libx264"}, threads: 1, preset: "fast"},
		observe: func(o EncodeObservation) { observed = o }}, nil); err != nil {
		t.Fatal(err)
	}
	if !observed.Succeeded || math.Abs(observed.OutputSeconds-pl.duration) > 0.2 {
		t.Fatalf("output seconds must reflect selected video: %+v (video %.2fs, container %.2fs)", observed, pl.duration, containerDuration)
	}
}
