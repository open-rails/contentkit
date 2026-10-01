package video_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// Uploads past their Upload.Video limits fail before any encode run exists:
// the audit's sparse 4K file (H4: 2 real frames every 15 s under a declared
// 60 fps), a video and an audio file longer than MaxSeconds.
func TestVideoLimits(t *testing.T) {
	e := newEnv(t, opts{limits: &media.VideoLimits{MaxSeconds: 5}})
	e.start()
	dir := t.TempDir()
	sparse := filepath.Join(dir, "D.mkv")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=black:s=3840x2160:r=60", "-frames:v", "8",
		"-vf", "setpts='(floor(N/2)*15 + mod(N,2)/60)/TB'", "-fps_mode", "passthrough", "-c:v", "libx264", "-preset", "ultrafast", sparse).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	tone := filepath.Join(dir, "tone.wav")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:a", "pcm_s16le", "-y", tone).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	cases := []struct {
		path, typ, file, upload, code string
	}{
		{"source", "video/x-matroska", sparse, "source.mkv", media.CodeVideoOverBudget},
		{"source", "video/x-matroska", fixture{w: 640, h: 360, secs: 9}.make(t), "source.mkv", media.CodeVideoTooLong},
		{"audio/tone.wav", "audio/wav", tone, "audio/tone.wav", media.CodeVideoTooLong},
	}
	for i, c := range cases {
		e.ref = e.refOf("video", 30+i)
		e.put(c.path, c.typ, c.file, nil)
	}
	e.wait()
	for i, c := range cases {
		e.ref = e.refOf("video", 30+i)
		m := e.manifest()
		f := e.file(m, c.upload)
		if fail := f.Fail(); fail == nil || fail.Code != c.code || len(f.Pending) != 0 || len(m.Files) != 1 {
			t.Fatalf("%s: %+v failure %+v", c.file, f, f.Failed)
		}
		if c.code == media.CodeVideoTooLong && (f.Failed.Details == nil || f.Failed.Details.MaxSeconds != 5 || f.Failed.Details.Seconds < 6) {
			t.Fatalf("%s: details %+v", c.file, f.Failed.Details)
		}
	}
	var runs int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM "+e.schema+".encode_run").Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("%d encode runs (%v)", runs, err)
	}
	if f := e.failed(); len(f) != len(cases) {
		t.Fatalf("failures %+v", f)
	}
}
