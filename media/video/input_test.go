package video

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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
	if err := ladder(ctx, list, out, pl, encoding{codec: EncoderX264, threads: 1}, nil); err == nil {
		t.Fatal("concat list encoded")
	}
}
