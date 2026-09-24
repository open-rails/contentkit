package video_test

import (
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
)

type packet struct {
	PTS   string `json:"pts_time"`
	Flags string `json:"flags"`
}

func packets(t *testing.T, path string) []packet {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "packet=pts_time,flags", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct{ Packets []packet }
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p.Packets
}

// A ladder encoded in parallel chunks has the one-pass x264 ladder's frames,
// timestamps, keyframes, segments and audio alignment. NVENC runs where a
// probe encode works.
func TestChunkedLadderMatchesOnePass(t *testing.T) {
	t.Run("x264", func(t *testing.T) { testChunkedLadder(t, video.EncoderX264) })
	t.Run("nvenc", func(t *testing.T) { testChunkedLadder(t, video.EncoderNVENC) })
}

func testChunkedLadder(t *testing.T, encoder string) {
	requireFFmpeg(t)
	if encoder == video.EncoderNVENC {
		if _, err := video.New(video.Config{Store: s3test.Open(t).Store, Encoder: encoder}); err != nil {
			t.Skip(err)
		}
	}
	src := fixture{w: 1280, h: 720, secs: 13, rate: 30, audio: 1, subs: true, tone: 440}.make(t)
	encode := func(encoder string, parallel int) (*env, *media.HLS) {
		e := newEnv(t, nil, nil)
		var err error
		if e.encoder, err = video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(),
			Threads: 4, Parallel: parallel, Encoder: encoder}); err != nil {
			t.Fatal(err)
		}
		e.commit(t, src, media.OpInsert)
		if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}, nil); err != nil {
			t.Fatal(err)
		}
		m, _ := e.manifest(t)
		return e, m.Files[m.File("source")].HLS
	}
	oneEnv, one := encode(video.EncoderX264, 1)
	chunkedEnv, chunked := encode(encoder, 4) // chunks at 0, 4, 8 and 12 s
	if len(one.Video) != 2 || len(chunked.Video) != 2 {
		t.Fatalf("ladders %d and %d", len(one.Video), len(chunked.Video))
	}
	for i, r := range chunked.Video {
		path := chunkedEnv.blob(t, r.Blob)
		checkByteRanges(t, path, r.Segments, "video", 13)
		got, want := packets(t, path), packets(t, oneEnv.blob(t, one.Video[i].Blob))
		if len(got) != 390 || len(got) != len(want) {
			t.Fatalf("rung %d: %d packets, one pass %d, want 390", r.Rung, len(got), len(want))
		}
		var pts, keys []float64
		for _, p := range got {
			var v float64
			if err := json.Unmarshal([]byte(p.PTS), &v); err != nil {
				t.Fatal(err)
			}
			pts = append(pts, v)
			if p.Flags[0] == 'K' {
				keys = append(keys, math.Round(v*1000)/1000)
			}
		}
		var onePTS []float64
		for _, p := range want {
			var v float64
			_ = json.Unmarshal([]byte(p.PTS), &v)
			onePTS = append(onePTS, v)
		}
		slices.Sort(pts)
		slices.Sort(onePTS)
		shift := 0.0 // NVENC's B-frame delay may differ from x264's
		if encoder != video.EncoderX264 {
			shift = pts[0] - onePTS[0]
		}
		for j := range pts {
			if math.Abs(pts[j]-shift-onePTS[j]) > 1e-4 {
				t.Fatalf("rung %d frame %d at %.6f, one pass %.6f", r.Rung, j, pts[j], onePTS[j])
			}
		}
		base := math.Round(pts[0]*1000) / 1000
		for _, k := range []float64{0, 4, 8, 12} {
			if !slices.Contains(keys, base+k) {
				t.Fatalf("rung %d: no keyframe at %g s: %v", r.Rung, k, keys)
			}
		}
		if len(r.Segments) != len(one.Video[i].Segments) {
			t.Fatalf("rung %d: %d segments, one pass %d", r.Rung, len(r.Segments), len(one.Video[i].Segments))
		}
		for j, s := range r.Segments {
			if s.Seconds != one.Video[i].Segments[j].Seconds {
				t.Fatalf("rung %d segment %d: %gs, one pass %gs", r.Rung, j, s.Seconds, one.Video[i].Segments[j].Seconds)
			}
		}
	}
	a, b := packets(t, oneEnv.blob(t, one.Audio[0].Blob)), packets(t, chunkedEnv.blob(t, chunked.Audio[0].Blob))
	if len(a) != len(b) || a[0].PTS != b[0].PTS {
		t.Fatalf("audio: %d packets from %s, one pass %d from %s", len(b), b[0].PTS, len(a), a[0].PTS)
	}
	if len(chunked.Subs) != 1 || chunked.Sprite == nil || *chunked.Sprite != (media.Sprite{Blob: chunked.Sprite.Blob, Cols: 10, Rows: 10,
		Width: one.Sprite.Width, Height: one.Sprite.Height, Interval: one.Sprite.Interval}) {
		t.Fatalf("subs %+v sprite %+v, one pass %+v", chunked.Subs, chunked.Sprite, one.Sprite)
	}
	// The sprite's last tile is filled, not tile padding.
	sprite := chunkedEnv.blob(t, chunked.Sprite.Blob)
	out, err := exec.Command("ffmpeg", "-hide_banner", "-i", sprite, "-vf",
		"crop=iw/10:ih/10:iw*9/10:ih*9/10,signalstats,metadata=print:key=lavfi.signalstats.YAVG", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("sprite: %v: %s", err, out)
	}
	_, v, _ := strings.Cut(string(out), "YAVG=")
	v, _, _ = strings.Cut(v, "\n")
	if yavg, _ := strconv.ParseFloat(strings.TrimSpace(v), 64); yavg < 30 {
		t.Fatalf("last sprite tile is blank: %q", out)
	}
}
