package video

import (
	"bytes"
	"context"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
)

func TestParseFFmpegProgress(t *testing.T) {
	in := "frame=0\nout_time_us=N/A\nspeed=N/A\nprogress=continue\n" +
		"frame=12\nout_time_us=1500000\nout_time=00:00:01.500000\nspeed=2.1x\nprogress=continue\n" +
		"out_time_us=4000000\nprogress=end\n"
	var got []float64
	if err := parseFFmpegProgress(strings.NewReader(in), func(s float64) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 1.5 || got[1] != 4 {
		t.Fatalf("out times %v", got)
	}
}

func TestSegments(t *testing.T) {
	for d, want := range map[float64]int{0.5: 1, 4: 1, 4.01: 2, 60: 15, 60.02: 16, 107.3: 27} {
		if got := segments(d); got != want {
			t.Errorf("segments(%g) = %d, want %d", d, got, want)
		}
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *clock) seconds(s float64) *clock {
	c.advance(time.Duration(s * float64(time.Second)))
	return c
}

func TestProgressEncodingETA(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	var reports []map[string]media.EncodeProgress
	p := newProgress(context.Background(), func(_ context.Context, m map[string]media.EncodeProgress) {
		reports = append(reports, m)
	}, 2*time.Second, c.now, []string{"a", "b"})
	if len(reports) != 1 || reports[0]["a"].Phase != media.PhaseQueued || reports[0]["b"].Phase != media.PhaseQueued {
		t.Fatalf("initial %+v", reports)
	}
	f := p.file("a")
	f.moved(10<<20, false) // download
	c.seconds(1)
	f.moved(10<<20, false)
	f.set(media.PhaseProbing)
	f.probed(108, "")

	// 3× realtime; reports throttled to one per 2 s.
	before := len(reports)
	var etas []float64
	var last media.EncodeProgress
	for i := 1; i <= 60; i++ {
		c.seconds(0.5)
		f.encoded(float64(i) * 1.5)
		m := reports[len(reports)-1]
		if cur := m["a"]; cur.At != last.At {
			if cur.Percent < last.Percent || cur.SegmentsDone < last.SegmentsDone {
				t.Fatalf("regressed %+v after %+v", cur, last)
			}
			if cur.ETA > 0 {
				etas = append(etas, cur.ETA)
			}
			last = cur
		}
	}
	if n := len(reports) - before; n < 13 || n > 17 {
		t.Fatalf("%d reports over 30 s at a 2 s throttle", n)
	}
	if last.SegmentsTotal != 27 || last.SegmentsDone != 22 || last.Phase != media.PhaseEncoding {
		t.Fatalf("last %+v", last)
	}
	if math.Abs(last.Speed-3) > 0.2 {
		t.Fatalf("speed %v, want ≈3", last.Speed)
	}
	if len(etas) < 10 || etas[len(etas)-1] >= etas[0] {
		t.Fatalf("etas %v", etas)
	}
	for i := 1; i < len(etas); i++ {
		if etas[i] > etas[i-1] {
			t.Fatalf("eta rose at steady speed: %v", etas)
		}
	}
	// 18 media seconds at 3× plus 0 projected upload (no out dir): ≈ 6 s.
	if last.ETA < 5 || last.ETA > 8 {
		t.Fatalf("eta %v", last.ETA)
	}

	f.uploads(100 << 20)
	f.set(media.PhaseUploading)
	up := reports[len(reports)-1]["a"]
	if up.SegmentsDone != 27 || up.ETA <= 0 || up.Percent < last.Percent {
		t.Fatalf("uploading %+v", up)
	}
	f.set(media.PhasePublishing)
	if pub := reports[len(reports)-1]["a"]; pub.Percent != 99 || pub.ETA != 0 {
		t.Fatalf("publishing %+v", pub)
	}
	p.done("a")
	if m := reports[len(reports)-1]; len(m) != 1 || m["b"].Phase != media.PhaseQueued {
		t.Fatalf("after done %+v", m)
	}
}

func TestSmooth(t *testing.T) {
	avg := smooth(0, 4, 1)
	if avg != 4 {
		t.Fatalf("first sample %v", avg)
	}
	// A spike moves the average by 1-e^(-1/8) of the gap, not all the way.
	avg = smooth(avg, 12, 1)
	if want := 4 + (1-math.Exp(-1.0/8))*8; math.Abs(avg-want) > 1e-9 {
		t.Fatalf("avg %v, want %v", avg, want)
	}
}

func TestCountingReaderHighWater(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	var got int64
	p := newProgress(context.Background(), func(context.Context, map[string]media.EncodeProgress) {}, time.Hour, c.now, []string{"a"})
	f := p.file("a")
	f.uploads(1000)
	r := f.reader(bytes.NewReader(make([]byte, 1000)), true).(io.ReadSeeker)
	if _, err := io.Copy(io.Discard, r); err != nil { // signing pass
		t.Fatal(err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil { // send
		t.Fatal(err)
	}
	p.mu.Lock()
	got = f.upDone
	p.mu.Unlock()
	if got != 1000 {
		t.Fatalf("counted %d of 1000", got)
	}
}
