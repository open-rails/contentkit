//go:build bench

// Encode benchmark: go test -tags bench -run TestBenchEncode ./media/video -timeout 0 -v
//
//	CONTENTKIT_BENCH_SAMPLES   comma list of source files (see bench/samples.sh)
//	CONTENTKIT_BENCH_LABEL     row label, e.g. "before" or "after"
//	CONTENTKIT_BENCH_THREADS   Config.Threads (default: GOMAXPROCS)
//	CONTENTKIT_BENCH_TMP       Config.TempDir (default: a test temp dir)
//	(and the knobs in bench_knobs_test.go)
//	CONTENTKIT_BENCH_VMAF      an ffmpeg with libvmaf; unset scores SSIM/PSNR only
//	CONTENTKIT_BENCH_QUALITY   0 skips quality scoring
//	CONTENTKIT_BENCH_OUT       JSON lines appended per sample
//
// plus CONTENTKIT_TEST_URL and the s3test variables. Runs the queued plan,
// chunk and assembly workers on Linux. Each sample reports wall time, CPU seconds
// of the worker and its ffmpeg children, peak RSS (sum of live ffmpeg children;
// the worker), peak scratch bytes, cumulative job time and per-codec/rung quality
// and bitrate.
package video_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

type benchRung struct {
	Codec   media.Codec `json:"codec"`
	Rung    int         `json:"rung"`
	W       int         `json:"w"`
	H       int         `json:"h"`
	AvgKbps int         `json:"avg_kbps"`
	VMAF    float64     `json:"vmaf,omitempty"`
	VMAF1   float64     `json:"vmaf_p1,omitempty"` // 1% low
	VMAFMin float64     `json:"vmaf_min,omitempty"`
	SSIM    float64     `json:"ssim"`
	PSNR    float64     `json:"psnr"`
}

type benchResult struct {
	Label      string             `json:"label"`
	Sample     string             `json:"sample"`
	Duration   float64            `json:"duration_s"`
	Threads    int                `json:"threads"`
	Knobs      string             `json:"knobs,omitempty"`
	Playable   float64            `json:"first_playable_s,omitempty"` // first observed playable manifest (100 ms polling)
	Frames     int                `json:"frames"`
	CPUPerOut  float64            `json:"cpu_ms_per_output_frame"`
	FFmpeg     string             `json:"ffmpeg"`
	Load1      float64            `json:"load1"`
	Wall       float64            `json:"wall_s"`
	CPU        float64            `json:"cpu_s"`
	FFmpegRSS  int64              `json:"ffmpeg_peak_rss_mb"`
	WorkerRSS  int64              `json:"worker_peak_rss_mb"`
	TempPeak   int64              `json:"temp_peak_mb"`
	Phases     map[string]float64 `json:"phases_s"` // cumulative worker time by job kind
	Rungs      []benchRung        `json:"rungs"`
	Downloads  int64              `json:"downloads_mb"`
	Realtime   float64            `json:"x_realtime"`
	ConfigNote string             `json:"config,omitempty"`
}

func TestBenchEncode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("resource sampling requires Linux /proc")
	}
	videotest.RequireFFmpeg(t)
	samples := strings.Split(os.Getenv("CONTENTKIT_BENCH_SAMPLES"), ",")
	if samples[0] == "" {
		t.Skip("CONTENTKIT_BENCH_SAMPLES not set")
	}
	for _, s := range samples {
		t.Run(filepath.Base(s), func(t *testing.T) { benchSample(t, s) })
	}
}

func benchSample(t *testing.T, src string) {
	e := newEnv(t, opts{profile: os.Getenv("CONTENTKIT_BENCH_PROFILE"), mp4: []int{1080, 480}})
	cfg := video.Config{Manifests: e.ms, Queue: e.queue, TempDir: os.Getenv("CONTENTKIT_BENCH_TMP")}
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	cfg.Threads, _ = strconv.Atoi(os.Getenv("CONTENTKIT_BENCH_THREADS"))
	defer benchKnobs(&cfg)()
	var err error
	if e.enc, err = video.New(e.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e.wc.Encoder, e.wc.ChunkTarget = e.enc, 0
	path := benchCommit(t, e, src)

	res := benchResult{Label: os.Getenv("CONTENTKIT_BENCH_LABEL"), Sample: filepath.Base(src), Threads: cfg.Threads,
		FFmpeg: ffmpegVersion(), Load1: load1(), Phases: map[string]float64{}}
	if res.Threads == 0 {
		res.Threads = runtime.GOMAXPROCS(0)
	}
	runtime.GC()
	func() {
		stop := sampleResources(cfg.TempDir, &res)
		defer stop()
		start, cpu0 := time.Now(), cpuSeconds()
		e.start()
		defer e.stopWorker()
		jobTable := pgx.Identifier{e.schema, "river_job"}.Sanitize()
		for {
			select {
			case event := <-e.events:
				if event.Kind != river.EventKindJobCompleted {
					t.Fatalf("job %s %s: %+v", event.Kind, event.Job.Kind, event.Job.Errors)
				}
				continue
			case <-time.After(100 * time.Millisecond):
			}
			m := e.manifest()
			if res.Playable == 0 && len(m.Outputs(path, "hls")) > 0 {
				res.Playable = time.Since(start).Seconds()
			}
			if r := e.readiness(m); r.State == media.StateFailed {
				t.Fatalf("video failed: %+v", e.file(m, path).Failed)
			} else if !r.Ready() {
				continue
			}
			// Wait for every video job in this sample's private schema.
			rows, err := e.pool.Query(e.ctx, `SELECT kind, bool_and(state = 'completed'),
COALESCE(sum(extract(epoch FROM finalized_at - attempted_at)), 0)
FROM `+jobTable+` WHERE queue IN ($1, $2) GROUP BY kind`, workqueue.VideoLightQueue, workqueue.VideoEncodeQueue)
			if err != nil {
				t.Fatal(err)
			}
			done := true
			var kind string
			var completed bool
			var seconds float64
			if _, err = pgx.ForEachRow(rows, []any{&kind, &completed, &seconds}, func() error {
				done = done && completed
				res.Phases[kind] = seconds
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if done {
				res.Wall = time.Since(start).Seconds()
				res.CPU = cpuSeconds() - cpu0
				return
			}
		}
	}()

	m := e.manifest()
	var renditions []media.File
	for _, o := range m.Outputs(path, "hls") {
		if o.Track.Kind == media.TrackVideo {
			renditions = append(renditions, o)
		}
	}
	if len(renditions) == 0 {
		t.Fatal("no ladder")
	}
	res.Knobs = knobsNote()
	res.Duration = e.file(m, path).Dur
	res.Frames = frameCount(t, src)
	res.CPUPerOut = res.CPU * 1000 / float64(res.Frames*len(renditions))
	res.Realtime = res.Duration / res.Wall
	for _, f := range m.Files {
		if strings.HasPrefix(f.Preset, "mp4-") {
			res.Downloads += f.Size >> 20
		}
	}
	for _, r := range renditions {
		local := e.blob(r.Blob)
		rung, _ := strconv.Atoi(strings.TrimPrefix(strings.SplitN(r.Path, "-", 2)[0], "hls/"))
		br := benchRung{Codec: media.Codec(r.Track.Codec), Rung: rung, W: r.W, H: r.H, AvgKbps: r.Track.Average / 1000}
		if os.Getenv("CONTENTKIT_BENCH_QUALITY") != "0" {
			br.SSIM, br.PSNR, br.VMAF, br.VMAF1, br.VMAFMin = quality(t, src, local, r.W, r.H)
		}
		res.Rungs = append(res.Rungs, br)
		os.Remove(local)
	}
	b, _ := json.Marshal(res)
	t.Logf("%s", b)
	if out := os.Getenv("CONTENTKIT_BENCH_OUT"); out != "" {
		fh, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(fh, "%s\n", b)
		fh.Close()
	}
}

// benchCommit ingests and places the sample before encoding begins.
func benchCommit(t *testing.T, e *env, src string) string {
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	typ := map[string]string{".mkv": "video/x-matroska", ".mov": "video/quicktime"}[filepath.Ext(src)]
	if typ == "" {
		typ = "video/mp4"
	}
	result, err := e.up.Ingest(e.ctx, e.editor, media.IngestRequest{Ref: e.ref, Path: "source" + filepath.Ext(src),
		Type: typ, Body: f, Size: info.Size(), PartSize: 256 << 20, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ms.Place(e.ctx, e.ref); err != nil {
		t.Fatal(err)
	}
	for _, file := range result.Manifest.Files {
		if file.Staged == result.Staged {
			return file.Path
		}
	}
	t.Fatal("source not committed")
	return ""
}

var (
	ssimRe = regexp.MustCompile(`SSIM .*All:([0-9.]+)`)
	psnrRe = regexp.MustCompile(`PSNR .*average:([0-9.inf]+)`)
	vmafRe = regexp.MustCompile(`VMAF score: ([0-9.]+)`)
)

// quality scores the rendition against the source scaled to its size, so it
// measures the encoder rather than the downscale. Frames pair by index after
// skipping the rendition's leading frames that best align it (constant-rate
// HLS output may repeat the first frame of a jittery-timestamp source).
func quality(t *testing.T, src, dist string, w, h int) (ssim, psnr, vmaf, p1, low float64) {
	graph := func(skip, frames int) string {
		limit := ""
		if frames > 0 {
			limit = fmt.Sprintf(",trim=end_frame=%d", frames)
		}
		return fmt.Sprintf("[1:v]scale=%d:%d:flags=bicubic,setsar=1,format=yuv420p%s,settb=1/30,setpts=N[r];"+
			"[0:v]format=yuv420p,trim=start_frame=%d%s,settb=1/30,setpts=N[d]", w, h, limit, skip, limit)
	}
	score := func(ff, lavfi string) []byte {
		out, err := exec.Command("nice", "-n", "19", ff, "-nostdin", "-i", dist, "-i", src, "-lavfi", lavfi, "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", lavfi, err, out)
		}
		return out
	}
	skip, best := 0, -1.0
	for k := range 4 {
		if m := psnrRe.FindSubmatch(score("ffmpeg", graph(k, 60)+";[d][r]psnr")); m != nil {
			if v, _ := strconv.ParseFloat(string(m[1]), 64); v > best {
				skip, best = k, v
			}
		}
	}
	out := score("ffmpeg", graph(skip, 0)+";[d]split[d1][d2];[r]split[r1][r2];[d1][r1]ssim;[d2][r2]psnr")
	if m := ssimRe.FindSubmatch(out); m != nil {
		ssim, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	if m := psnrRe.FindSubmatch(out); m != nil {
		psnr, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	if ff := os.Getenv("CONTENTKIT_BENCH_VMAF"); ff != "" {
		log := filepath.Join(t.TempDir(), "vmaf.json")
		score(ff, graph(skip, 0)+fmt.Sprintf(";[d][r]libvmaf=n_threads=%d:n_subsample=3:log_fmt=json:log_path=%s", min(8, runtime.NumCPU()), log))
		var v struct {
			Frames []struct {
				Metrics struct {
					VMAF float64 `json:"vmaf"`
				} `json:"metrics"`
			} `json:"frames"`
		}
		b, err := os.ReadFile(log)
		if err != nil || json.Unmarshal(b, &v) != nil || len(v.Frames) == 0 {
			t.Fatalf("vmaf log: %v", err)
		}
		scores := make([]float64, len(v.Frames))
		for i, f := range v.Frames {
			scores[i] = f.Metrics.VMAF
			vmaf += f.Metrics.VMAF
		}
		slices.Sort(scores)
		vmaf /= float64(len(scores))
		p1, low = scores[len(scores)/100], scores[0]
	}
	return ssim, psnr, vmaf, p1, low
}

func frameCount(t *testing.T, src string) int {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets", "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", src).Output()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func cpuSeconds() float64 {
	var self, kids syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &self)
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids)
	tv := func(v syscall.Timeval) float64 { return float64(v.Sec) + float64(v.Usec)/1e6 }
	return tv(self.Utime) + tv(self.Stime) + tv(kids.Utime) + tv(kids.Stime)
}

// sampleResources polls child RSS, the worker's RSS and scratch size until stop.
func sampleResources(tmp string, res *benchResult) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pid := strconv.Itoa(os.Getpid())
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			var kids int64
			procs, _ := os.ReadDir("/proc")
			for _, p := range procs {
				if _, err := strconv.Atoi(p.Name()); err != nil {
					continue
				}
				st, err := os.ReadFile("/proc/" + p.Name() + "/stat")
				if err != nil {
					continue
				}
				_, rest, _ := strings.Cut(string(st), ") ")
				if fields := strings.Fields(rest); len(fields) > 1 && fields[1] == pid {
					kids += statusKB(p.Name(), "VmRSS")
				}
			}
			res.FFmpegRSS = max(res.FFmpegRSS, kids>>10)
			res.WorkerRSS = max(res.WorkerRSS, statusKB("self", "VmRSS")>>10)
			var used int64
			_ = filepath.WalkDir(tmp, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					if info, err := d.Info(); err == nil {
						used += info.Size()
					}
				}
				return nil
			})
			res.TempPeak = max(res.TempPeak, used>>20)
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

func statusKB(pid, key string) int64 {
	f, err := os.Open("/proc/" + pid + "/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+":"); ok {
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return n
		}
	}
	return 0
}

func load1() float64 {
	b, _ := os.ReadFile("/proc/loadavg")
	v, _ := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	return v
}

func ffmpegVersion() string {
	out, _ := exec.Command("ffmpeg", "-version").Output()
	f := strings.Fields(string(out))
	if len(f) > 2 {
		return f[2]
	}
	return ""
}
