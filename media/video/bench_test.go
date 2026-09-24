//go:build bench

// Encode benchmark: go test -tags bench -run TestBenchEncode ./media/video -timeout 0 -v
//
//	CONTENTKIT_BENCH_SAMPLES   comma list of source files (see bench/samples.sh)
//	CONTENTKIT_BENCH_LABEL     row label, e.g. "before" or "after"
//	CONTENTKIT_BENCH_THREADS   Config.Threads (default: GOMAXPROCS)
//	CONTENTKIT_BENCH_TMP       Config.TempDir (default: a test temp dir)
//	CONTENTKIT_BENCH_ENCODER   Config.Encoder
//	CONTENTKIT_BENCH_NVENC_CQ  NVENC -cq override
//	CONTENTKIT_BENCH_VMAF      an ffmpeg with libvmaf; unset scores SSIM/PSNR only
//	CONTENTKIT_BENCH_QUALITY   0 skips quality scoring
//	CONTENTKIT_BENCH_OUT       JSON lines appended per sample
//
// plus the s3test variables. Each sample reports wall time, CPU seconds of
// the worker and its ffmpeg children, peak RSS (sum of live ffmpeg children;
// the worker), peak scratch bytes, per-phase seconds and per-rung quality and
// bitrate.
package video_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
)

type benchRung struct {
	Rung    int     `json:"rung"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	AvgKbps int     `json:"avg_kbps"`
	VMAF    float64 `json:"vmaf,omitempty"`
	SSIM    float64 `json:"ssim"`
	PSNR    float64 `json:"psnr"`
}

type benchResult struct {
	Label      string             `json:"label"`
	Sample     string             `json:"sample"`
	Duration   float64            `json:"duration_s"`
	Threads    int                `json:"threads"`
	Encoder    string             `json:"encoder,omitempty"`
	FFmpeg     string             `json:"ffmpeg"`
	Load1      float64            `json:"load1"`
	Wall       float64            `json:"wall_s"`
	CPU        float64            `json:"cpu_s"`
	FFmpegRSS  int64              `json:"ffmpeg_peak_rss_mb"`
	WorkerRSS  int64              `json:"worker_peak_rss_mb"`
	TempPeak   int64              `json:"temp_peak_mb"`
	Phases     map[string]float64 `json:"phases_s"`
	Rungs      []benchRung        `json:"rungs"`
	Downloads  int64              `json:"downloads_mb"`
	Realtime   float64            `json:"x_realtime"`
	ConfigNote string             `json:"config,omitempty"`
}

func TestBenchEncode(t *testing.T) {
	requireFFmpeg(t)
	samples := strings.Split(os.Getenv("CONTENTKIT_BENCH_SAMPLES"), ",")
	if samples[0] == "" {
		t.Skip("CONTENTKIT_BENCH_SAMPLES not set")
	}
	for _, s := range samples {
		t.Run(filepath.Base(s), func(t *testing.T) { benchSample(t, s) })
	}
}

func benchSample(t *testing.T, src string) {
	s3 := s3test.Open(t)
	kinds, err := media.NewRegistry(media.Kind{Name: "video", Versioned: true, Video: &media.Video{}, Types: []string{"video/x-matroska", "video/mp4", "video/quicktime"}})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.NewManifests(s3.Store, kinds, media.ManifestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: s3.Store, Kinds: kinds, Manifests: ms, Authorizer: grants{}})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.NewVersion(s3.Tenant, "video", "1", "v1")
	item, _ := kinds.Item(ref)
	ctx := context.Background()

	name := benchCommit(t, ctx, s3.Store, item, src)
	if _, err := uploads.Commit(ctx, admin, ref, []media.Op{{Op: "insert", Name: "source", Original: name}}); err != nil {
		t.Fatal(err)
	}

	cfg := video.Config{Store: s3.Store, TempDir: os.Getenv("CONTENTKIT_BENCH_TMP")}
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	cfg.Threads, _ = strconv.Atoi(os.Getenv("CONTENTKIT_BENCH_THREADS"))
	cfg.Encoder = os.Getenv("CONTENTKIT_BENCH_ENCODER")
	if cq := os.Getenv("CONTENTKIT_BENCH_NVENC_CQ"); cq != "" {
		defer video.SetNVENCCQ(cq)()
	}
	enc, err := video.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	res := benchResult{Label: os.Getenv("CONTENTKIT_BENCH_LABEL"), Sample: filepath.Base(src), Threads: cfg.Threads, Encoder: cfg.Encoder,
		FFmpeg: ffmpegVersion(), Load1: load1(), Phases: map[string]float64{}}
	if res.Threads == 0 {
		res.Threads = runtime.GOMAXPROCS(0)
	}
	runtime.GC()
	stop := sampleResources(cfg.TempDir, &res)
	var mu sync.Mutex
	phase, at := "", time.Now()
	report := func(_ context.Context, files map[string]media.EncodeProgress) {
		mu.Lock()
		defer mu.Unlock()
		p, ok := files["source"]
		if !ok {
			p, ok = files[media.ItemProgressKey]
		}
		if !ok || p.Phase == phase {
			return
		}
		now := time.Now()
		if phase != "" {
			res.Phases[phase] += now.Sub(at).Seconds()
		}
		phase, at = p.Phase, now
	}
	cpu0 := cpuSeconds()
	start := time.Now()
	if err := enc.Encode(ctx, video.Job{Ref: ref, Versioned: true}, report); err != nil {
		t.Fatal(err)
	}
	res.Wall = time.Since(start).Seconds()
	res.CPU = cpuSeconds() - cpu0
	stop()
	mu.Lock()
	if phase != "" {
		res.Phases[phase] += time.Since(at).Seconds()
	}
	mu.Unlock()

	m, _, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f := m.Files[m.File("source")]
	if f.HLS == nil || f.HLS.Error != "" || len(f.HLS.Video) == 0 {
		t.Fatalf("no ladder: %+v", f.HLS)
	}
	res.Duration, _ = f.Meta["duration"].(float64)
	res.Realtime = res.Duration / res.Wall
	for _, d := range m.Downloads {
		res.Downloads += d.Size >> 20
	}
	dir := t.TempDir()
	for _, r := range f.HLS.Video {
		path := benchFetch(t, ctx, s3.Store, item, r.Blob, dir)
		br := benchRung{Rung: r.Rung, W: r.Width, H: r.Height, AvgKbps: r.Average / 1000}
		if os.Getenv("CONTENTKIT_BENCH_QUALITY") != "0" {
			br.SSIM, br.PSNR, br.VMAF = quality(t, src, path, r.Width, r.Height)
		}
		res.Rungs = append(res.Rungs, br)
		os.Remove(path)
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

func benchCommit(t *testing.T, ctx context.Context, store media.Store, item media.Item, src string) string {
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	sum := h.Sum(nil)
	name := media.SHA256Name(sum)
	key, _ := item.Original(name)
	if size > 1<<30 {
		id, err := store.CreateMultipart(ctx, key, "video/mp4")
		if err != nil {
			t.Fatal(err)
		}
		var parts []media.Part
		for off, n := int64(0), int32(1); off < size; off, n = off+256<<20, n+1 {
			l := min(256<<20, size-off)
			ph := sha256.New()
			_, _ = io.Copy(ph, io.NewSectionReader(f, off, l))
			p, err := store.PutPart(ctx, key, id, n, io.NewSectionReader(f, off, l), l, ph.Sum(nil))
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, p)
		}
		if _, err := store.CompleteMultipart(ctx, key, id, parts); err != nil {
			t.Fatal(err)
		}
		return name
	}
	if _, err := store.Put(ctx, key, io.NewSectionReader(f, 0, size), size, media.PutOptions{ContentType: "video/mp4", ChecksumSHA256: sum}); err != nil {
		t.Fatal(err)
	}
	return name
}

func benchFetch(t *testing.T, ctx context.Context, store media.Store, item media.Item, blob, dir string) string {
	key, _ := item.Blob(blob)
	rc, _, err := store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	path := filepath.Join(dir, blob+".mp4")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return path
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
func quality(t *testing.T, src, dist string, w, h int) (ssim, psnr, vmaf float64) {
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
		out := score(ff, graph(skip, 0)+fmt.Sprintf(";[d][r]libvmaf=n_threads=%d:n_subsample=3", min(16, runtime.NumCPU())))
		if m := vmafRe.FindSubmatch(out); m != nil {
			vmaf, _ = strconv.ParseFloat(string(m[1]), 64)
		}
	}
	return ssim, psnr, vmaf
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
