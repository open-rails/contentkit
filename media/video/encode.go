package video

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/open-rails/contentkit/media"
)

// recipe is the encode's identity with the ladder: a manifest hls or
// download whose spec differs is stale and re-encoded.
const recipe = "h264-high-crf22-fast|k4|short-side:%s|max-4096-3840x2160|lvl51-52|max60fps|sar1|aac-128k-48k-2ch|webvtt|sprite-10x10-short90-jpg|mp4-all-audio-mov_text|v2"

// Spec identifies the recipe over ladder (empty: media.DefaultLadder) in
// manifest hls and downloads entries.
func Spec(ladder []int) string {
	if len(ladder) == 0 {
		ladder = media.DefaultLadder
	}
	h := make([]string, len(ladder))
	for i, v := range ladder {
		h[i] = strconv.Itoa(v)
	}
	s := sha256.Sum256(fmt.Appendf(nil, recipe, strings.Join(h, ",")))
	return hex.EncodeToString(s[:4])
}

// sourceDemuxers are the containers a source may be; playlists, concat lists,
// image sequences and devices never reach ffmpeg.
var sourceDemuxers = []string{"mov", "matroska", "avi", "mpegts", "flv", "ogg", "asf", "mpeg"}

// inputOptions confine the next input to local files of the given demuxers.
func inputOptions(demuxers []string) []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", strings.Join(demuxers, ",")}
}

const (
	segmentSeconds = 4
	spriteCols     = 10
	spriteRows     = 10
	spriteShort    = 90
)

// ladder runs the single ffmpeg pass: the source is decoded once and split
// into every rendition, audio track, subtitle and the sprite.
func ladder(ctx context.Context, src, dir string, p plan, enc encoding, fp *fileProgress) error {
	n := len(p.rungs)
	interval := p.duration / (spriteCols * spriteRows)
	var fc strings.Builder
	fmt.Fprintf(&fc, "[0:%d]", p.video)
	if p.limitFPS {
		fmt.Fprintf(&fc, "fps=%d,", maxFPS)
	}
	fmt.Fprintf(&fc, "split=%d", n+1)
	for i := range n + 1 {
		fmt.Fprintf(&fc, "[s%d]", i)
	}
	for i, r := range p.rungs {
		fmt.Fprintf(&fc, ";[s%d]scale=%d:%d,setsar=1[v%d]", i, r.w, r.h, i)
	}
	// Sprite frames leave the pass as PNGs and are tiled after: a tile filter
	// emits only at the end, and ffmpeg reports no progress until every
	// output has started.
	fmt.Fprintf(&fc, ";[s%d]fps=1/%.9f,scale=%d:%d,setsar=1[sprite]", n, interval, p.tileW, p.tileH)

	// Decoding and scaling gain little past 8 threads; each extra frame
	// thread holds more decoded frames.
	t := strconv.Itoa(min(enc.threads, 8))
	args := append([]string{"-v", "error", "-nostdin", "-threads", t}, inputOptions(sourceDemuxers)...)
	args = append(args, "-i", src, "-filter_complex_threads", t, "-filter_complex", fc.String())
	hls := func(segment, playlist string) []string {
		return []string{"-fflags", "+bitexact", "-flags", "+bitexact", "-muxdelay", "0", "-muxpreload", "0",
			"-f", "hls", "-hls_time", strconv.Itoa(segmentSeconds), "-hls_playlist_type", "vod",
			"-hls_segment_type", "fmp4", "-hls_flags", "single_file", "-hls_segment_filename", segment, playlist}
	}
	var streamMap []string
	for i := range n {
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
		streamMap = append(streamMap, fmt.Sprintf("v:%d", i))
	}
	args = append(args, codecArgs(enc.codec)...)
	args = append(args, "-pix_fmt", "yuv420p", "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds))
	for i, r := range p.rungs {
		if enc.codec == EncoderX264 {
			args = append(args, fmt.Sprintf("-threads:v:%d", i), strconv.Itoa(rungThreads(p.rungs, i, enc.threads)))
		}
		if r.level != "" {
			args = append(args, fmt.Sprintf("-level:v:%d", i), r.level)
		}
	}
	args = append(args, "-var_stream_map", strings.Join(streamMap, " "))
	args = append(args, hls(filepath.Join(dir, "v%v.mp4"), filepath.Join(dir, "v%v.m3u8"))...)
	for i, a := range p.audio {
		args = append(args, "-map", fmt.Sprintf("0:%d", a.index), "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
		args = append(args, hls(filepath.Join(dir, fmt.Sprintf("a%d.mp4", i)), filepath.Join(dir, fmt.Sprintf("a%d.m3u8", i)))...)
	}
	for i, s := range p.subs {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.index), "-c:s", "webvtt", "-f", "webvtt", filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)))
	}
	args = append(args, "-map", "[sprite]", "-frames:v", strconv.Itoa(spriteCols*spriteRows), "-f", "image2", filepath.Join(dir, spriteFrames))
	if err := ffmpegProgress(ctx, fp.encoded, args...); err != nil {
		return err
	}
	return sprite(ctx, dir)
}

// rungThreads is rung i's share of threads by frame area, at least 2 (a
// single-threaded x264 has no lookahead thread and stalls the pass). Giving
// every rung all threads costs a 4K ladder a third more CPU and memory for a
// slower pass (bench_test.go).
func rungThreads(rungs []rung, i, threads int) int {
	var total float64
	for _, r := range rungs {
		total += float64(r.w * r.h)
	}
	return max(min(threads, 2), int(math.Ceil(float64(threads)*float64(rungs[i].w*rungs[i].h)/total)))
}

// Config.Encoder values.
const (
	EncoderAuto  = "auto"  // NVENC when a probe encode succeeds, else x264
	EncoderX264  = "x264"  // libx264 on the CPU
	EncoderNVENC = "nvenc" // NVIDIA h264_nvenc; decoding and scaling stay on the CPU
)

// encoding is how a file's ladder is encoded.
type encoding struct {
	codec   string // EncoderX264 or EncoderNVENC
	threads int
}

// codecArgs are the recipe's H.264 High settings for codec.
func codecArgs(codec string) []string {
	if codec == EncoderNVENC {
		return []string{"-c:v", "h264_nvenc", "-profile:v", "high", "-preset", "p5", "-tune", "hq", "-rc", "vbr", "-cq", nvencCQ,
			"-b:v", "0", "-spatial-aq", "1", "-temporal-aq", "1", "-rc-lookahead", "20", "-bf", "3", "-b_ref_mode", "middle", "-forced-idr", "1"}
	}
	return []string{"-c:v", "libx264", "-profile:v", "high", "-preset", "fast", "-crf", "22"}
}

// nvencCQ matches x264 CRF 22's VMAF on the top rung; lower rungs cost NVENC 30-60% more bits (bench_test.go).
var nvencCQ = "26"

// nvencWorks encodes one frame with h264_nvenc.
func nvencWorks(ctx context.Context) error {
	_, err := command(ctx, "ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=s=256x256:d=0.1", "-frames:v", "1",
		"-c:v", "h264_nvenc", "-f", "null", "-")
	return err
}

const spriteFrames = "t%03d.png"

// sprite tiles the pass's frames into sprite.jpg and removes them.
func sprite(ctx context.Context, dir string) error {
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions([]string{"image2"})...)
	args = append(args, "-framerate", "1", "-i", filepath.Join(dir, spriteFrames),
		"-vf", fmt.Sprintf("tile=%dx%d", spriteCols, spriteRows), "-frames:v", "1", "-q:v", "4", "-f", "image2", "-y", filepath.Join(dir, "sprite.jpg"))
	if _, err := command(ctx, "ffmpeg", args...); err != nil {
		return err
	}
	frames, err := filepath.Glob(filepath.Join(dir, "t*.png"))
	for _, f := range frames {
		err = errors.Join(err, os.Remove(f))
	}
	return err
}

// mux stream-copies one rendition, every audio track (default first) and the
// subtitles into a faststart MP4 download. Output is byte-identical on retry.
func mux(ctx context.Context, dir string, rendition int, p plan, out string) error {
	own := inputOptions([]string{"mov", "webvtt"}) // our own renditions and subtitles
	args := append([]string{"-v", "error", "-nostdin"}, own...)
	args = append(args, "-i", filepath.Join(dir, fmt.Sprintf("v%d.mp4", rendition)))
	order := make([]int, 0, len(p.audio))
	for i, a := range p.audio {
		if a.def {
			order = append([]int{i}, order...)
		} else {
			order = append(order, i)
		}
	}
	for _, i := range order {
		args = append(append(args, own...), "-i", filepath.Join(dir, fmt.Sprintf("a%d.mp4", i)))
	}
	for i := range p.subs {
		args = append(append(args, own...), "-i", filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)))
	}
	args = append(args, "-map", "0:v:0")
	in := 1
	for o, i := range order {
		a := p.audio[i]
		args = append(args, "-map", fmt.Sprintf("%d:a:0", in), fmt.Sprintf("-metadata:s:a:%d", o), "language="+a.iso6392,
			fmt.Sprintf("-metadata:s:a:%d", o), "handler_name="+a.label, fmt.Sprintf("-disposition:a:%d", o), map[bool]string{true: "default", false: "0"}[o == 0])
		in++
	}
	for o, s := range p.subs {
		args = append(args, "-map", fmt.Sprintf("%d:s:0", in), fmt.Sprintf("-metadata:s:s:%d", o), "language="+s.iso6392,
			fmt.Sprintf("-metadata:s:s:%d", o), "handler_name="+s.label, fmt.Sprintf("-disposition:s:%d", o), "0")
		in++
	}
	args = append(args, "-c", "copy", "-c:s", "mov_text", "-map_metadata", "-1", "-map_chapters", "-1",
		"-fflags", "+bitexact", "-flags:v", "+bitexact", "-flags:a", "+bitexact", "-movflags", "+faststart", "-y", out)
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

// playlist is a parsed single-file fMP4 media playlist.
type playlist struct {
	init     int64 // init segment length at offset 0
	segments []media.Segment
}

// parsePlaylist reads ffmpeg's byte-range playlist and requires the file to be
// exactly [init][segment 0][segment 1]…, so a player can derive the init
// segment as bytes [0, segments[0].Offset).
func parsePlaylist(path string, fileSize int64) (playlist, error) {
	var pl playlist
	f, err := os.Open(path)
	if err != nil {
		return pl, err
	}
	defer f.Close()
	var seconds float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			_, br, ok := strings.Cut(line, `BYTERANGE="`)
			if !ok {
				return pl, fmt.Errorf("%s: init segment without byte range", path)
			}
			length, offset, err := byteRange(strings.TrimSuffix(br, `"`))
			if err != nil || offset != 0 {
				return pl, fmt.Errorf("%s: init segment %q", path, br)
			}
			pl.init = length
		case strings.HasPrefix(line, "#EXTINF:"):
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			if seconds, err = strconv.ParseFloat(v, 64); err != nil || seconds <= 0 || math.IsInf(seconds, 0) {
				return pl, fmt.Errorf("%s: segment duration %q", path, v)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			length, offset, err := byteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
			if err != nil || seconds <= 0 {
				return pl, fmt.Errorf("%s: segment %q", path, line)
			}
			pl.segments = append(pl.segments, media.Segment{Offset: offset, Length: length, Seconds: math.Round(seconds*1e6) / 1e6})
			seconds = 0
		}
	}
	if err := sc.Err(); err != nil {
		return pl, err
	}
	if pl.init <= 0 || len(pl.segments) == 0 {
		return pl, fmt.Errorf("%s: no init segment or segments", path)
	}
	next := pl.init
	for _, s := range pl.segments {
		if s.Offset != next || s.Length <= 0 {
			return pl, fmt.Errorf("%s: segments are not contiguous at offset %d", path, s.Offset)
		}
		next += s.Length
	}
	if next != fileSize {
		return pl, fmt.Errorf("%s: segments cover %d of %d bytes", path, next, fileSize)
	}
	return pl, nil
}

func byteRange(v string) (length, offset int64, err error) {
	l, o, ok := strings.Cut(v, "@")
	if !ok {
		return 0, 0, errors.New("byte range without offset")
	}
	if length, err = strconv.ParseInt(l, 10, 64); err == nil {
		offset, err = strconv.ParseInt(o, 10, 64)
	}
	return length, offset, err
}

// bandwidth is the peak and average bits per second over the segments.
func bandwidth(segs []media.Segment) (peak, avg int) {
	var bits, secs, top float64
	for _, s := range segs {
		b := float64(s.Length * 8)
		top = math.Max(top, b/s.Seconds)
		bits += b
		secs += s.Seconds
	}
	return int(math.Ceil(top)), int(math.Ceil(bits / secs))
}

// avcCodec is the RFC 6381 codecs value of an encoded rendition.
func avcCodec(ctx context.Context, path string) (codec string, w, h int, err error) {
	p, err := probe(ctx, path)
	if err != nil {
		return "", 0, 0, err
	}
	for _, s := range p.Streams {
		if s.CodecType != "video" || s.CodecName != "h264" {
			continue
		}
		profile := map[string]string{"High": "6400", "Main": "4d40", "Constrained Baseline": "42e0", "Baseline": "4200"}[s.Profile]
		if profile == "" || s.Level <= 0 || s.Level > 255 {
			return "", 0, 0, fmt.Errorf("%s: unsupported H.264 profile %q level %d", path, s.Profile, s.Level)
		}
		return fmt.Sprintf("avc1.%s%02x", profile, s.Level), s.Width, s.Height, nil
	}
	return "", 0, 0, fmt.Errorf("%s: no H.264 stream", path)
}

// command runs a tool, killing it (SIGTERM, then SIGKILL) when ctx ends.
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	if err := run(ctx, &out, name, args...); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ffmpegProgress runs ffmpeg with -progress on stdout, passing each report's
// out_time in seconds to fn.
func ffmpegProgress(ctx context.Context, fn func(outTime float64), args ...string) error {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = parseFFmpegProgress(pr, fn)
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := run(ctx, pw, "ffmpeg", append([]string{"-progress", "pipe:1", "-nostats"}, args...)...)
	_ = pw.Close()
	<-done
	return err
}

func run(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var stderr tail
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w: %s", name, err, stderr.b)
	}
	return nil
}

// tail keeps the last 16 KiB of a tool's diagnostics.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 16<<10 {
		t.b = t.b[len(t.b)-16<<10:]
	}
	return len(p), nil
}
