package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/open-rails/contentkit/media"
)

// Frames are cut from a rendition when one is current: the init segment
// plus the segments covering the time are one small local fMP4, so nothing
// downloads the source and ffmpeg reads only our own output.
var ownMP4 = inputOptions([]string{"mov"})

// frameInput is an ffmpeg input to cut a frame from at offset seconds.
type frameInput struct {
	path   string
	opts   []string // input options: ownMP4, or the source's remote options
	offset float64
}

// args open the input; -ss before -i decodes from the previous keyframe and
// drops frames before the offset, so the first frame out is the one there.
func (in frameInput) args(threads int) []string {
	args := append([]string{"-v", "error", "-nostdin", "-threads", strconv.Itoa(threads)}, in.opts...)
	return append(args, "-ss", strconv.FormatFloat(math.Max(0, in.offset), 'f', 6, 64), "-i", in.path, "-map", "0:v:0", "-frames:v", "1")
}

// snippet writes the init segment and the segments of the byte-range track
// at key covering [from, to] to path; start is the time the first of them
// begins.
func snippet(ctx context.Context, store media.Store, key string, segs []media.Segment, from, to float64, path string) (start float64, err error) {
	if len(segs) == 0 {
		return 0, errors.New("media/video: rendition has no segments")
	}
	first, last := -1, len(segs)-1
	var t float64
	for i, s := range segs {
		if first < 0 && (from < t+s.Seconds || i == last) {
			first, start = i, t
		}
		if to < t+s.Seconds { // a time on a boundary starts the next segment
			last = max(i, first)
			break
		}
		t += s.Seconds
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	err = copyRange(ctx, store, f, key, 0, segs[0].Offset)
	if err == nil {
		end := segs[last].Offset + segs[last].Length
		err = copyRange(ctx, store, f, key, segs[first].Offset, end-segs[first].Offset)
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

// grabFrame writes the input's frame, scaled to w×h, as a PNG.
func grabFrame(ctx context.Context, in frameInput, w, h int, out string, threads int) error {
	args := append(in.args(threads), "-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos,setsar=1", w, h), "-c:v", "png", "-f", "image2", "-y", out)
	if _, err := command(ctx, "ffmpeg", args...); err != nil {
		return err
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return fmt.Errorf("no frame at %.3fs", in.offset)
	}
	return nil
}

// detail is the luma standard deviation (0-255) of the input's frame,
// measured on a 64×36 thumbnail: near zero for black and flat frames.
func detail(ctx context.Context, in frameInput, threads int) (float64, error) {
	out, err := command(ctx, "ffmpeg", append(in.args(threads), "-vf", "scale=64:36,format=gray", "-f", "rawvideo", "pipe:1")...)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("no frame at %.3fs", in.offset)
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

// frameJPEG decodes the input's frame as a JPEG width px wide (0: the
// frame's own width).
func frameJPEG(ctx context.Context, in frameInput, width int) ([]byte, error) {
	out, err := command(ctx, "ffmpeg", append(in.args(1), "-vf", fmt.Sprintf("scale=%d:-2:flags=bicubic,setsar=1", width),
		"-q:v", "4", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1")...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no frame at %.3fs", in.offset)
	}
	return out, nil
}
