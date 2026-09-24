// Package videotest builds synthetic videos whose frames identify their time
// and orientation, and classifies decoded pixels, for poster and preview tests.
package videotest

import (
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RequireFFmpeg skips without ffmpeg and ffprobe, or fails under
// CONTENTKIT_TEST_FFMPEG=1 (the CI jobs that install them).
func RequireFFmpeg(t testing.TB) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
}

// Segments writes a 12 s 640×360 MP4: black (flat) until 3 s, then a white
// grid over red [3,5), lime [5,7), blue [7,9), yellow [9,11) and magenta,
// with a cyan box over the bottom-right quadrant from 3 s. rotate sets the
// display rotation (degrees counter-clockwise; 90 turns the cyan box to the
// top right of a 360×640 picture).
func Segments(t testing.TB, rotate int) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "segments.mp4")
	vf := "color=c=black:s=640x360:r=10:d=12," +
		"drawbox=c=red:t=fill:enable='gte(t,3)',drawbox=c=lime:t=fill:enable='gte(t,5)',drawbox=c=blue:t=fill:enable='gte(t,7)'," +
		"drawbox=c=yellow:t=fill:enable='gte(t,9)',drawbox=c=magenta:t=fill:enable='gte(t,11)'," +
		"drawgrid=w=40:h=40:t=2:c=white:enable='gte(t,3)',drawbox=x=iw/2:y=ih/2:w=iw/2:h=ih/2:c=cyan:t=fill:enable='gte(t,3)'"
	Run(t, "ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", vf, "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1",
		"-pix_fmt", "yuv420p", "-y", src)
	if rotate == 0 {
		return src
	}
	out := filepath.Join(dir, "rotated.mp4")
	Run(t, "ffmpeg", "-v", "error", "-nostdin", "-display_rotation", fmt.Sprint(rotate), "-i", src, "-c", "copy", "-y", out)
	return out
}

// Run runs a tool and returns its stdout, failing the test on error.
func Run(t testing.TB, name string, args ...string) []byte {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("%s: %v: %s", name, err, ee.Stderr)
		}
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

var palette = map[string][3]int{"black": {0, 0, 0}, "red": {255, 0, 0}, "lime": {0, 255, 0}, "blue": {0, 0, 255},
	"yellow": {255, 255, 0}, "magenta": {255, 0, 255}, "cyan": {0, 255, 255}, "white": {255, 255, 255}}

func nearest(r, g, b int) string {
	best, name := math.MaxInt, ""
	for n, c := range palette {
		d := (r-c[0])*(r-c[0]) + (g-c[1])*(g-c[1]) + (b-c[2])*(b-c[2])
		if d < best {
			best, name = d, n
		}
	}
	return name
}

// Dominant is the most common palette colour in rect, grid lines aside.
func Dominant(img image.Image, rect image.Rectangle) string {
	counts := map[string]int{}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if n := nearest(int(r>>8), int(g>>8), int(b>>8)); n != "white" {
				counts[n]++
			}
		}
	}
	best, name := -1, ""
	for n, c := range counts {
		if c > best {
			best, name = c, n
		}
	}
	return name
}

// Quadrant is the dominant colour of a quadrant: 0 top-left, 1 top-right,
// 2 bottom-left, 3 bottom-right.
func Quadrant(img image.Image, q int) string {
	b := img.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	x, y := b.Min.X+(q%2)*w, b.Min.Y+(q/2)*h
	return Dominant(img, image.Rect(x+w/8, y+h/8, x+w-w/8, y+h-h/8))
}
