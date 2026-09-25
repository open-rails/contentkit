package video

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestPassthroughChecks(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	cadence := []string{"-force_key_frames", "expr:gte(t,n_forced*4)", "-sc_threshold", "0"}
	x265 := func(params string, more ...string) []string {
		return append([]string{"-c:v", "libx265", "-preset", "ultrafast", "-crf", "28", "-x265-params", params + ":log-level=error"}, more...)
	}
	hevc := x265("open-gop=0:scenecut=0", "-tag:v", "hvc1", "-forced-idr", "1", "-force_key_frames", "expr:gte(t,n_forced*4)")
	for _, c := range []struct {
		name, ext string
		args      []string
		want      string // "" passes; else a fragment of the reason
		codec     media.Codec
	}{
		{"compliant", "mp4", cadence, "", media.CodecH264},
		{"matroska", "mkv", cadence, "container", media.CodecH264},
		{"scene-cut keyframes only", "mp4", nil, "no keyframe", media.CodecH264},
		{"open GOP", "mp4", []string{"-x264-params", "open-gop=1:keyint=60:min-keyint=60:scenecut=0"}, "not an IDR", media.CodecH264},
		{"4:4:4", "mp4", append([]string{"-pix_fmt", "yuv444p", "-profile:v", "high444"}, cadence...), "codec", media.CodecH264},
		{"over the cap", "mp4", append([]string{"-crf", "4"}, cadence...), "over the rung", media.CodecH264},
		{"HEVC compliant", "mp4", hevc, "", media.CodecHEVC},
		{"HEVC hev1", "mp4", x265("open-gop=0:scenecut=0", "-tag:v", "hev1", "-forced-idr", "1", "-force_key_frames", "expr:gte(t,n_forced*4)"), "codec", media.CodecHEVC},
		{"HEVC open GOP", "mp4", x265("open-gop=1:keyint=120:min-keyint=120:scenecut=0", "-tag:v", "hvc1"), "not an IDR", media.CodecHEVC},
	} {
		src := filepath.Join(dir, c.name+"."+c.ext)
		args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=9", "-f", "lavfi", "-i", "sine=duration=9",
			"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p"}
		args = append(append(args, c.args...), "-c:a", "aac", "-y", src)
		if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", c.name, err, b)
		}
		pr, err := probe(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		p, err := newPlan(pr, &media.Video{})
		if err != nil {
			t.Fatal(err)
		}
		codec, ok, why := passthroughable(ctx, src, p, p.rungs[0])
		if c.want == "" && !ok || c.want != "" && (ok || !strings.Contains(why, c.want)) || codec != c.codec {
			t.Errorf("%s: passthrough %v %s (%s), want %q %s", c.name, ok, codec, why, c.want, c.codec)
		}
	}
}
