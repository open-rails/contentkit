package video

import (
	"errors"
	"fmt"
	"math"
)

// Frame caps: every rung decodes on common H.264 hardware (level 5.1/5.2
// decoders take at most 4096 px per side and a 3840×2160 frame area).
const (
	maxSide = 4096
	maxArea = 3840 * 2160
	maxFPS  = 60
)

// ErrAspect marks a source whose display aspect is outside the kind's
// bounds; it is permanent and reported through Hooks.Failed.
var ErrAspect = errors.New("aspect ratio out of range")

// aspectSlack admits sources a pixel of rounding off the bounds.
const aspectSlack = 0.005

// rung is one output of the ladder: label n (the short side asked for)
// and the encoded frame.
type rung struct {
	n, w, h int
	level   string // H.264 level; "" lets x264 choose
}

// checkAspect refuses a w×h display outside [lo, hi].
func checkAspect(w, h int, lo, hi float64) error {
	a := float64(w) / float64(h)
	if a < lo*(1-aspectSlack) || a > hi*(1+aspectSlack) {
		return fmt.Errorf("%w: %dx%d is %.3f, allowed %.3f–%.3f", ErrAspect, w, h, a, lo, hi)
	}
	return nil
}

// frame is the even output size of rung n for a w×h display: the short
// side is n, then the frame shrinks (aspect kept) to fit maxSide and maxArea.
func frame(w, h, n int) (int, int) {
	short, long := float64(min(w, h)), float64(max(w, h))
	f := min(float64(n)/short, maxSide/long, math.Sqrt(maxArea/(short*long)))
	s, l := min(even(short*f), int(short)&^1), min(even(long*f), int(long)&^1)
	for l > maxSide || s*l > maxArea {
		if l > maxSide || float64(l)*short >= float64(s)*long {
			l -= 2
		} else {
			s -= 2
		}
	}
	if w < h {
		return s, l
	}
	return l, s
}

func even(v float64) int { return max(2, 2*int(math.Round(v/2))) }

// rungs is the ladder for a w×h display at fps: rungs above the short side
// are dropped (a source below the lowest rung is encoded once at its own
// short side), and a rung whose capped frame repeats the next one's is
// dropped.
func rungs(ladder []int, w, h int, fps float64) []rung {
	short := min(w, h)
	var ns []int
	for _, n := range ladder {
		if n <= short {
			ns = append(ns, n)
		}
	}
	if len(ns) == 0 {
		ns = []int{max(2, short-short%2)}
	}
	var out []rung
	for i, n := range ns {
		rw, rh := frame(w, h, n)
		if i+1 < len(ns) {
			if nw, nh := frame(w, h, ns[i+1]); nw == rw && nh == rh {
				continue
			}
		}
		out = append(out, rung{n: n, w: rw, h: rh, level: level(rw, rh, fps)})
	}
	return out
}

// level is the lowest H.264 level 5.x (Annex A: max frame size and
// macroblock rate) that holds a frame beyond 1080p-class (level 4.1's 8192
// macroblocks); smaller frames keep x264's own choice.
func level(w, h int, fps float64) string {
	mbs := float64(((w + 15) / 16) * ((h + 15) / 16))
	if mbs <= 8192 {
		return ""
	}
	for _, l := range []struct {
		name     string
		fs, rate float64
	}{{"5.0", 22080, 589824}, {"5.1", 36864, 983040}} {
		if mbs <= l.fs && mbs*fps <= l.rate {
			return l.name
		}
	}
	return "5.2"
}

// tile is a sprite tile for a w×h display: short side spriteShort, aspect kept.
func tile(w, h int) (int, int) {
	long := even(float64(spriteShort) * float64(max(w, h)) / float64(min(w, h)))
	if w < h {
		return spriteShort, long
	}
	return long, spriteShort
}
