package video_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// A compliant source's top rung in its own codec is its video stream,
// copied: the same frames, on the lower rung's segments, switchable with it.
// The other codec's top rung is encoded. HEVC (libx265) is configured for
// the HEVC source and tagged hvc1.
func TestPassthroughTopRung(t *testing.T) {
	for _, c := range []struct {
		codec, other media.Codec
		prefix       string
		args         []string
	}{
		{media.CodecH264, media.CodecAV1, "avc1.64", []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-sc_threshold", "0"}},
		{media.CodecHEVC, media.CodecH264, "hvc1.1.6.L", []string{"-c:v", "libx265", "-preset", "ultrafast", "-crf", "30", "-tag:v", "hvc1", "-forced-idr", "1",
			"-x265-params", "open-gop=0:scenecut=0:log-level=error"}},
	} {
		t.Run(string(c.codec), func(t *testing.T) {
			e := newEnv(t, opts{codecs: []media.Codec{c.codec, c.other}})
			src := filepath.Join(t.TempDir(), "source.mp4")
			args := append([]string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=9", "-f", "lavfi", "-i", "sine=duration=9",
				"-map", "0:v", "-map", "1:a", "-pix_fmt", "yuv420p", "-force_key_frames", "expr:gte(t,n_forced*4)"}, c.args...)
			if b, err := exec.Command("ffmpeg", append(args, "-c:a", "aac", "-y", src)...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v: %s", err, b)
			}
			e.start()
			e.put("source", "video/mp4", src, nil)
			e.wait()
			var planned string
			if err := e.pool.QueryRow(e.ctx, "SELECT passthrough_codec FROM "+e.schema+".encode_run WHERE rung = 720").Scan(&planned); err != nil || planned != string(c.codec) {
				t.Fatalf("top rung planned passthrough %q: %v", planned, err)
			}
			decoded := func(path string) string {
				b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:v:0", "-an", "-pix_fmt", "yuv420p", "-f", "md5", "-").CombinedOutput()
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			m := e.manifest()
			for _, codec := range []media.Codec{c.codec, c.other} {
				top, low := e.file(m, "hls/720-"+string(codec)+".mp4"), e.file(m, "hls/480-"+string(codec)+".mp4")
				paths := []string{e.blob(top.Blob), e.blob(low.Blob)}
				if copied := decoded(paths[0]) == decoded(src); copied != (codec == c.codec) {
					t.Fatalf("%s 720 rung copied from the source: %v", codec, copied)
				}
				if codec == c.codec && !strings.HasPrefix(top.Track.Codecs, c.prefix) {
					t.Fatalf("%s codecs %q", codec, top.Track.Codecs)
				}
				segs := e.index(top).Segments
				checkByteRanges(t, paths[0], segs, "video", 9)
				switchRungs(t, paths, [][]media.Segment{segs, e.index(low).Segments})
			}
		})
	}
}
