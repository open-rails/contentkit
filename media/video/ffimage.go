package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"

	"github.com/open-rails/contentkit/media"
)

// Posters are cut from an HLS rendition, not the source: the
// init segment plus the segments covering the section are one small local
// fMP4, so nothing downloads the source and ffmpeg reads only our own output.
var ownMP4 = inputOptions([]string{"mov"})

// snippet writes the init segment and the segments of r covering [from, to]
// to path; start is the time the first of them begins.
func snippet(ctx context.Context, store media.Store, item media.Item, r media.Rendition, from, to float64, path string) (start float64, err error) {
	if len(r.Segments) == 0 {
		return 0, errors.New("media/video: rendition has no segments")
	}
	first, last := -1, len(r.Segments)-1
	var t float64
	for i, s := range r.Segments {
		if first < 0 && (from < t+s.Seconds || i == last) {
			first, start = i, t
		}
		if to < t+s.Seconds { // a time on a boundary starts the next segment
			last = max(i, first)
			break
		}
		t += s.Seconds
	}
	key, err := item.Blob(r.Blob)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	err = copyRange(ctx, store, f, key, 0, r.Segments[0].Offset)
	if err == nil {
		end := r.Segments[last].Offset + r.Segments[last].Length
		err = copyRange(ctx, store, f, key, r.Segments[first].Offset, end-r.Segments[first].Offset)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return start, err
}

func copyRange(ctx context.Context, store media.Store, w io.Writer, key string, off, n int64) error {
	rc, _, err := store.Get(ctx, key, media.GetOptions{Range: fmt.Sprintf("bytes=%d-%d", off, off+n-1)})
	if err != nil {
		return err
	}
	defer rc.Close()
	got, err := io.Copy(w, rc)
	if err == nil && got != n {
		err = fmt.Errorf("media/video: read %d of %d bytes of %s", got, n, key)
	}
	return err
}

// narrowest is the narrowest rendition at least width wide, else the widest.
func narrowest(f media.File, width int) media.Rendition {
	ladder := slices.SortedFunc(slices.Values(f.HLS.Video), func(a, b media.Rendition) int { return a.Width - b.Width })
	if i := slices.IndexFunc(ladder, func(v media.Rendition) bool { return v.Width >= width }); i >= 0 {
		return ladder[i]
	}
	return ladder[len(ladder)-1]
}

// seek is an ffmpeg input position; -ss before -i decodes from the previous
// keyframe and drops frames before t, so the first frame out is the one at t.
func seek(t float64) string { return strconv.FormatFloat(math.Max(0, t), 'f', 6, 64) }

func ffmpegIn(threads int, pre ...string) []string {
	args := append([]string{"-v", "error", "-nostdin", "-threads", strconv.Itoa(threads)}, ownMP4...)
	return append(args, pre...)
}

// grabFrame writes the frame at offset into input, scaled to w×h, as a PNG.
func grabFrame(ctx context.Context, input string, offset float64, w, h int, out string, threads int) error {
	args := append(ffmpegIn(threads, "-ss", seek(offset), "-i", input), "-map", "0:v:0", "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos,setsar=1", w, h), "-c:v", "png", "-f", "image2", "-y", out)
	if _, err := command(ctx, "ffmpeg", args...); err != nil {
		return err
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return fmt.Errorf("no frame at %.3fs", offset)
	}
	return nil
}

// detail is the luma standard deviation (0-255) of the frame at offset,
// measured on a 64×36 thumbnail: near zero for black and flat frames.
func detail(ctx context.Context, input string, offset float64, threads int) (float64, error) {
	args := append(ffmpegIn(threads, "-ss", seek(offset), "-i", input), "-map", "0:v:0", "-frames:v", "1",
		"-vf", "scale=64:36,format=gray", "-f", "rawvideo", "pipe:1")
	out, err := command(ctx, "ffmpeg", args...)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("no frame at %.3fs", offset)
	}
	var sum, sq float64
	for _, v := range out {
		sum += float64(v)
		sq += float64(v) * float64(v)
	}
	n := float64(len(out))
	mean := sum / n
	return math.Sqrt(math.Max(0, sq/n-mean*mean)), nil
}

// Automatic posters sample these fractions of the duration.
var autoPosterAt = []float64{0.2, 0.35, 0.5, 0.65, 0.8}

// minDetail is the luma std-dev below which a frame counts as black or flat.
const minDetail = 12

// clampTime keeps t off the container's end, past the last frame's start.
func clampTime(t, duration float64) float64 {
	return math.Round(math.Min(math.Max(0, t), math.Max(0, duration-0.25))*1000) / 1000
}

// frameJPEG decodes the frame at offset into input as a JPEG width px wide.
func frameJPEG(ctx context.Context, input string, offset float64, width int) ([]byte, error) {
	args := append(ffmpegIn(1, "-ss", seek(offset), "-i", input), "-map", "0:v:0", "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2:flags=bicubic,setsar=1", width), "-q:v", "4", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1")
	out, err := command(ctx, "ffmpeg", args...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no frame at %.3fs", offset)
	}
	return out, nil
}
