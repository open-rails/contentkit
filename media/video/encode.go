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

// recipe is the encode's identity with the ladder and profile: a manifest
// hls or download whose spec differs is stale and re-encoded.
const recipe = "codecs:%s|h264-high,hevc-main-hvc1-x265-closed,av1-svt-p%d|capped-crf:%s|k4-sc0|short-side:%s|native-below-%d|lanczos|max-4096-3840x2160|lvl51-52|max60fps|sar1|aac-128k-48k-2ch|webvtt|sprite-10x10-short90-jpg|mp4-%s-all-audio-mov_text|stage-per-rung|pt-top|v5"

// Spec identifies the recipe over v's ladder (empty: media.DefaultLadder)
// and profile and the encoder's codecs in manifest hls and downloads
// entries. The encoders (CPU or NVENC) are not part of it.
func (e *Encoder) Spec(v media.Video) string {
	ladder := v.Rungs()
	h := make([]string, len(ladder))
	for i, n := range ladder {
		h[i] = strconv.Itoa(n)
	}
	var rs []string
	for _, c := range e.c.Codecs {
		for _, n := range []int{2160, 1440, 1080, 720, 480} {
			r := rung{n: n, profile: v.Profile}.rate(c)
			rs = append(rs, fmt.Sprintf("%d/%d", r.crf, r.maxrate))
		}
	}
	if v.Profile != "" {
		rs = append(rs, "tune-"+v.Profile)
	}
	codecs := make([]string, len(e.c.Codecs))
	for i, c := range e.c.Codecs {
		codecs[i] = string(c)
	}
	s := sha256.Sum256(fmt.Appendf(nil, recipe, strings.Join(codecs, ","), svtAV1Preset, strings.Join(rs, ","), strings.Join(h, ","),
		nativeBelow, e.downloadCodec()))
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
	segmentSeconds = 4 // also the keyframe interval: one IDR starts each segment
	spriteCols     = 10
	spriteRows     = 10
	spriteShort    = 90
)

// pass is one ffmpeg run of a file's stage: its rung in codecs, and whether
// it also makes the sprite frames.
type pass struct {
	rung     rung
	codecs   []media.Codec
	sprite   bool
	noTracks bool // audio and subtitles are already encoded
	enc      encoding
	observe  func(EncodeObservation)
}

// renditionName is a rendition's file name stem in a pass's directory.
func renditionName(n int, c media.Codec) string { return fmt.Sprintf("v%d-%s", n, c) }

// ladder runs one ffmpeg pass: the source is decoded once and scaled
// (lanczos) to the pass's rung, which is encoded in each codec and also
// feeds the sprite. Every audio track and subtitle is encoded alongside, for
// the downloads' mux.
func ladder(ctx context.Context, src, dir string, p plan, ps pass, fp *fileProgress) error {
	var fc strings.Builder
	fmt.Fprintf(&fc, "[0:%d]", p.video)
	if p.limitFPS {
		fmt.Fprintf(&fc, "fps=%d,", maxFPS)
	}
	var outs []string
	for i := range ps.codecs {
		outs = append(outs, fmt.Sprintf("[v%d]", i))
	}
	if ps.sprite {
		outs = append(outs, "[sp]")
	}
	fmt.Fprintf(&fc, "scale=%d:%d:flags=lanczos,setsar=1", ps.rung.w, ps.rung.h)
	if len(outs) > 1 {
		fmt.Fprintf(&fc, ",split=%d", len(outs))
	}
	fc.WriteString(strings.Join(outs, ""))
	if ps.sprite {
		// Sprite frames leave the pass as PNGs and are tiled after: a tile filter
		// emits only at the end, and ffmpeg reports no progress until every
		// output has started.
		fmt.Fprintf(&fc, ";[sp]fps=1/%.9f,scale=%d:%d:flags=lanczos,setsar=1[sprite]", p.duration/(spriteCols*spriteRows), p.tileW, p.tileH)
	}

	// Decoding and scaling gain little past 8 threads; each extra frame
	// thread holds more decoded frames.
	t := strconv.Itoa(min(ps.enc.threads, 8))
	// -y: a CPU retry after NVENC failed overwrites the failed pass's outputs.
	args := append([]string{"-v", "error", "-nostdin", "-y", "-threads", t}, inputOptions(sourceDemuxers)...)
	args = append(args, "-i", src)
	if len(outs) > 0 {
		args = append(args, "-filter_complex_threads", t, "-filter_complex", fc.String())
	}
	// One muxer per rendition: a muxer shifts all its streams by the most
	// negative DTS (x264's and x265's B-frame delay), which would move an AV1
	// rendition sharing it and misalign it with its rungs from other passes.
	for i, c := range ps.codecs {
		v := filepath.Join(dir, renditionName(ps.rung.n, c))
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
		args = append(args, ps.enc.streamArgs(0, ps.rung, c)...)
		args = append(args, "-pix_fmt", "yuv420p", "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds))
		args = append(args, hlsArgs(v+".mp4", v+".m3u8")...)
	}
	for i, a := range p.audio {
		if ps.noTracks {
			break
		}
		args = append(args, "-map", fmt.Sprintf("0:%d", a.index), "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
		args = append(args, hlsArgs(filepath.Join(dir, fmt.Sprintf("a%d.mp4", i)), filepath.Join(dir, fmt.Sprintf("a%d.m3u8", i)))...)
	}
	for i, s := range p.subs {
		if ps.noTracks {
			break
		}
		args = append(args, "-map", fmt.Sprintf("0:%d", s.index))
		args = append(args, webvttArgs(filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)))...)
	}
	if ps.sprite {
		args = append(args, "-map", "[sprite]", "-frames:v", strconv.Itoa(spriteCols*spriteRows), "-f", "image2", filepath.Join(dir, spriteFrames))
	}
	started := time.Now()
	cpu, err := ffmpegProgress(ctx, fp.encoded, args...)
	if ps.observe != nil {
		outputSeconds := 0.0
		if err == nil {
			outputSeconds = p.duration * float64(len(ps.codecs))
		}
		ps.observe(EncodeObservation{SourceClass: sourceClass(p.width, p.height), Duration: time.Since(started),
			CPU: cpu, OutputSeconds: outputSeconds, Succeeded: err == nil})
	}
	if err != nil {
		return err
	}
	if !ps.sprite {
		return nil
	}
	return sprite(ctx, dir)
}

func sourceClass(width, height int) string {
	switch {
	case max(width, height) <= 720:
		return "sd"
	case max(width, height) <= 1920:
		return "hd"
	default:
		return "uhd"
	}
}

func hlsArgs(segment, playlist string) []string {
	return []string{"-fflags", "+bitexact", "-flags", "+bitexact", "-muxdelay", "0", "-muxpreload", "0",
		"-f", "hls", "-hls_time", strconv.Itoa(segmentSeconds), "-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4", "-hls_flags", "single_file", "-hls_segment_filename", segment, playlist}
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

// mux stream-copies one rendition (the file stem v in dir), every audio
// track (default first) and the subtitles into a faststart MP4 download.
// Output is byte-identical on retry.
func mux(ctx context.Context, dir, v string, p plan, out string) error {
	own := inputOptions([]string{"mov", "webvtt"}) // our own renditions and subtitles
	args := append([]string{"-v", "error", "-nostdin"}, own...)
	args = append(args, "-i", filepath.Join(dir, v+".mp4"))
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

// renditionFrame is an encoded rendition's frame size.
func renditionFrame(ctx context.Context, path string) (w, h int, err error) {
	p, err := probe(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			return s.Width, s.Height, nil
		}
	}
	return 0, 0, fmt.Errorf("%s: no video stream", path)
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
func ffmpegProgress(ctx context.Context, fn func(outTime float64), args ...string) (time.Duration, error) {
	_, cpu, err := ffmpegProgressTail(ctx, fn, args...)
	return cpu, err
}

// ffmpegProgressTail is ffmpegProgress returning the tail of ffmpeg's stderr.
func ffmpegProgressTail(ctx context.Context, fn func(outTime float64), args ...string) ([]byte, time.Duration, error) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = parseFFmpegProgress(pr, fn)
		_, _ = io.Copy(io.Discard, pr)
	}()
	stderr, cpu, err := runTail(ctx, pw, "ffmpeg", append([]string{"-progress", "pipe:1", "-nostats"}, args...)...)
	_ = pw.Close()
	<-done
	return stderr, cpu, err
}

func run(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	_, _, err := runTail(ctx, stdout, name, args...)
	return err
}

// runTail runs a tool, returning the tail of its stderr.
func runTail(ctx context.Context, stdout io.Writer, name string, args ...string) ([]byte, time.Duration, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var stderr tail
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	var cpu time.Duration
	if cmd.ProcessState != nil {
		cpu = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, cpu, ctx.Err()
		}
		return nil, cpu, fmt.Errorf("%s: %w: %s", name, err, stderr.b)
	}
	return stderr.b, cpu, nil
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
