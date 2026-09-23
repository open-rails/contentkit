package video

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

// Recipe is the encode's identity: a manifest hls or download whose spec
// differs is stale and re-encoded.
const Recipe = "h264-high-crf22-fast|k4|2160,1440,1080,720,480|aac-128k-48k-2ch|webvtt|sprite-10x10-160x90-jpg|mp4-all-audio-mov_text|v1"

const (
	segmentSeconds = 4
	spriteCols     = 10
	spriteRows     = 10
	spriteW        = 160
	spriteH        = 90
)

// ladder runs the single ffmpeg pass: the source is decoded once and split
// into every rendition, audio track, subtitle and the sprite.
func ladder(ctx context.Context, src, dir string, p plan, threads int) error {
	n := len(p.rungs)
	interval := p.duration / (spriteCols * spriteRows)
	var fc strings.Builder
	fmt.Fprintf(&fc, "[0:%d]split=%d", p.video, n+1)
	for i := range n + 1 {
		fmt.Fprintf(&fc, "[s%d]", i)
	}
	for i, h := range p.rungs {
		fmt.Fprintf(&fc, ";[s%d]scale=-2:%d[v%d]", i, h, i)
	}
	fmt.Fprintf(&fc, ";[s%d]fps=1/%.9f,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,tile=%dx%d[sprite]",
		n, interval, spriteW, spriteH, spriteW, spriteH, spriteCols, spriteRows)

	t := strconv.Itoa(threads)
	args := []string{"-v", "error", "-nostdin", "-threads", t, "-i", src, "-filter_complex_threads", t, "-filter_complex", fc.String()}
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
	args = append(args, "-c:v", "libx264", "-profile:v", "high", "-preset", "fast", "-crf", "22", "-pix_fmt", "yuv420p",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds), "-threads", t,
		"-var_stream_map", strings.Join(streamMap, " "))
	args = append(args, hls(filepath.Join(dir, "v%v.mp4"), filepath.Join(dir, "v%v.m3u8"))...)
	for i, a := range p.audio {
		args = append(args, "-map", fmt.Sprintf("0:%d", a.index), "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
		args = append(args, hls(filepath.Join(dir, fmt.Sprintf("a%d.mp4", i)), filepath.Join(dir, fmt.Sprintf("a%d.m3u8", i)))...)
	}
	for i, s := range p.subs {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.index), "-c:s", "webvtt", "-f", "webvtt", filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)))
	}
	args = append(args, "-map", "[sprite]", "-frames:v", "1", "-q:v", "4", "-f", "image2", filepath.Join(dir, "sprite.jpg"))
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

// mux stream-copies one rendition, every audio track (default first) and the
// subtitles into a faststart MP4 download. Output is byte-identical on retry.
func mux(ctx context.Context, dir string, rendition int, p plan, out string) error {
	args := []string{"-v", "error", "-nostdin", "-i", filepath.Join(dir, fmt.Sprintf("v%d.mp4", rendition))}
	order := make([]int, 0, len(p.audio))
	for i, a := range p.audio {
		if a.def {
			order = append([]int{i}, order...)
		} else {
			order = append(order, i)
		}
	}
	for _, i := range order {
		args = append(args, "-i", filepath.Join(dir, fmt.Sprintf("a%d.mp4", i)))
	}
	for i := range p.subs {
		args = append(args, "-i", filepath.Join(dir, fmt.Sprintf("s%d.vtt", i)))
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
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var stderr tail
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, stderr.b)
	}
	return out, nil
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
