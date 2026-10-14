package video

import (
	"bufio"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/contentkit/media"
)

// Report receives a job's per-file encode progress, at most once per
// Config.ProgressInterval except on phase changes. A finished file leaves
// the map.
type Report func(ctx context.Context, files map[string]media.EncodeProgress)

const (
	speedTau = 8 * time.Second // smoothing time constant of speed and throughput
	// defaultRate estimates upload throughput until a transfer is measured.
	defaultRate = 20 << 20
	minMeasured = 8 << 20
)

// progress tracks one Encode's files; nil reports nothing.
type progress struct {
	ctx      context.Context
	report   Report
	interval time.Duration
	now      func() time.Time

	mu    sync.Mutex
	files map[string]*fileProgress
	last  time.Time
}

func newProgress(ctx context.Context, report Report, interval time.Duration, now func() time.Time, names []string) *progress {
	if report == nil {
		return nil
	}
	p := &progress{ctx: ctx, report: report, interval: interval, now: now, files: map[string]*fileProgress{}}
	for _, n := range names {
		p.files[n] = &fileProgress{p: p, phase: media.PhaseQueued}
	}
	p.emit(true)
	return p
}

// file starts a file's run; a nil progress yields a nil tracker.
func (p *progress) file(name string) *fileProgress {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	f := p.files[name]
	if f == nil {
		f = &fileProgress{p: p}
		p.files[name] = f
	}
	*f = fileProgress{p: p, phase: media.PhaseDownloading, start: p.now()}
	p.mu.Unlock()
	p.emit(true)
	return f
}

// item reports an item-wide phase until the returned func runs.
func (p *progress) item(phase string) func() {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	p.files[media.ItemProgressKey] = &fileProgress{p: p, phase: phase, start: p.now()}
	p.mu.Unlock()
	p.emit(true)
	return func() { p.done(media.ItemProgressKey) }
}

// done drops a finished or failed file.
func (p *progress) done(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.files, name)
	p.mu.Unlock()
	p.emit(true)
}

func (p *progress) emit(force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !force && now.Sub(p.last) < p.interval {
		return
	}
	p.last = now
	out := make(map[string]media.EncodeProgress, len(p.files))
	for n, f := range p.files {
		out[n] = f.snapshot(now)
	}
	p.report(p.ctx, out)
}

// fileProgress is one file's run. Its methods are nil-safe and called under
// no lock; p.mu guards the fields.
type fileProgress struct {
	p     *progress
	phase string
	start time.Time

	duration float64 // probed seconds
	outDir   string  // ffmpeg outputs, measured to project upload bytes
	outTime  float64 // encoded media seconds
	speed    float64 // smoothed ×realtime
	lastWall time.Time
	lastOut  float64
	started  bool

	xferBytes       int64 // finished fetches and uploads
	xferTime        time.Duration
	upTotal, upDone int64
	percent         float64
	stageN, stages  int
}

func (f *fileProgress) set(phase string) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	f.phase = phase
	f.p.mu.Unlock()
	f.p.emit(true)
}

// stage records which encode stage the run is.
func (f *fileProgress) stage(n, of int) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	f.stageN, f.stages = n, of
	f.p.mu.Unlock()
}

// probed records the duration and output directory before encoding.
func (f *fileProgress) probed(duration float64, outDir string) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	f.duration, f.outDir, f.phase = duration, outDir, media.PhaseEncoding
	f.lastWall, f.lastOut = f.p.now(), 0
	f.p.mu.Unlock()
	f.p.emit(true)
}

// encoded records ffmpeg's out_time.
func (f *fileProgress) encoded(outTime float64) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	now := f.p.now()
	// The first report only sets the baseline: ffmpeg's start-up would read as a crawl.
	if !f.started {
		f.started, f.lastWall, f.lastOut = true, now, outTime
	} else if dt := now.Sub(f.lastWall).Seconds(); dt >= 0.25 && outTime > f.lastOut {
		f.speed = smooth(f.speed, (outTime-f.lastOut)/dt, dt)
		f.lastWall, f.lastOut = now, outTime
	}
	f.outTime = max(f.outTime, min(outTime, f.duration))
	f.p.mu.Unlock()
	f.p.emit(false)
}

// uploads fixes the bytes left to upload once encoding has finished.
func (f *fileProgress) uploads(total int64) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	f.outTime, f.upTotal, f.upDone = f.duration, total, 0
	f.p.mu.Unlock()
}

// moved counts bytes of an upload in flight.
func (f *fileProgress) moved(n int64) {
	if f == nil || n <= 0 {
		return
	}
	f.p.mu.Lock()
	f.upDone += n
	f.p.mu.Unlock()
	f.p.emit(false)
}

// transferred records a finished fetch or upload: throughput is whole
// transfers' bytes over their wall time, so latency and an SDK's signing
// pass count and a lucky first read does not.
func (f *fileProgress) transferred(n int64, d time.Duration) {
	if f == nil || n <= 0 || d <= 0 {
		return
	}
	f.p.mu.Lock()
	f.xferBytes += n
	f.xferTime += d
	f.p.mu.Unlock()
}

func (f *fileProgress) skipped(n int64) {
	if f == nil {
		return
	}
	f.p.mu.Lock()
	f.upDone += n
	f.p.mu.Unlock()
}

// reader counts an upload's bytes. A seekable body stays seekable (the S3
// client rewinds to sign and retry); only its high-water mark counts.
func (f *fileProgress) reader(r io.Reader) io.Reader {
	if f == nil {
		return r
	}
	c := &countingReader{r: r, f: f}
	if s, ok := r.(io.ReadSeeker); ok {
		return &countingReadSeeker{countingReader: c, s: s}
	}
	return c
}

type countingReader struct {
	r        io.Reader
	f        *fileProgress
	pos, top int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if c.pos += int64(n); c.pos > c.top {
		c.f.moved(c.pos - c.top)
		c.top = c.pos
	}
	return n, err
}

type countingReadSeeker struct {
	*countingReader
	s io.ReadSeeker
}

func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	pos, err := c.s.Seek(off, whence)
	if err == nil {
		c.pos = pos
	}
	return pos, err
}

// snapshot is the wire progress at now; p.mu is held.
func (f *fileProgress) snapshot(now time.Time) media.EncodeProgress {
	out := media.EncodeProgress{Phase: f.phase, At: now.UnixMilli(), Stage: f.stageN, Stages: f.stages}
	if f.duration > 0 {
		out.SegmentsTotal = segments(f.duration)
		out.SegmentsDone = min(out.SegmentsTotal, int(f.outTime/segmentSeconds))
		if f.outTime >= f.duration {
			out.SegmentsDone = out.SegmentsTotal
		}
	}
	rate := float64(defaultRate)
	if f.xferBytes >= minMeasured {
		rate = float64(f.xferBytes) / f.xferTime.Seconds()
	}
	var eta float64
	switch f.phase {
	case media.PhaseEncoding:
		if f.speed > 0 {
			out.Speed = math.Round(f.speed*100) / 100
			eta = (f.duration-f.outTime)/f.speed + float64(f.projectedUpload())/rate
		}
	case media.PhaseMuxing, media.PhaseUploading:
		eta = float64(max(0, f.upTotal-f.upDone)) / rate
	case media.PhasePublishing:
		eta, f.percent = 0, max(f.percent, 99)
	}
	if eta > 0 {
		out.ETA = math.Ceil(eta)
		elapsed := now.Sub(f.start).Seconds()
		f.percent = max(f.percent, min(99, 100*elapsed/(elapsed+eta)))
	}
	out.Percent = math.Round(f.percent*10) / 10
	return out
}

// projectedUpload scales the renditions written so far to the whole
// duration, doubled for the muxed downloads.
func (f *fileProgress) projectedUpload() int64 {
	frac := f.outTime / f.duration
	if f.outDir == "" || frac < 0.1 {
		return 0
	}
	var n int64
	for _, pat := range []string{"v*.mp4", "a*.mp4"} {
		matches, _ := filepath.Glob(filepath.Join(f.outDir, pat))
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil {
				n += st.Size()
			}
		}
	}
	return int64(2 * float64(n) / frac)
}

// segments is the HLS segment count of a duration: -force_key_frames cuts
// every segmentSeconds, the last one short.
func segments(duration float64) int {
	return max(1, int(math.Ceil(duration/segmentSeconds-1e-6)))
}

// smooth is an exponential moving average over time: a sample dt seconds
// after the last weighs 1-e^(-dt/τ).
func smooth(avg, sample, dt float64) float64 {
	if avg <= 0 {
		return sample
	}
	return avg + (1-math.Exp(-dt/speedTau.Seconds()))*(sample-avg)
}

// parseFFmpegProgress reads `-progress` key=value blocks and calls fn with
// each block's out_time in seconds; blocks without a time are skipped.
func parseFFmpegProgress(r io.Reader, fn func(outTime float64)) error {
	sc := bufio.NewScanner(r)
	t := -1.0
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_us":
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 {
				t = float64(us) / 1e6
			}
		case "progress":
			if t >= 0 {
				fn(t)
			}
			t = -1
		}
	}
	return sc.Err()
}
