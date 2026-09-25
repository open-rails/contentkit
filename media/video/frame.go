package video

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/open-rails/contentkit/media"
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
	level   string // H.264 level; "" lets the encoder choose
	profile string // media.Video.Profile
}

// rungRate is a rung's capped CRF. The cap bounds bitrate spikes (Apple's
// HLS spec wants a VOD peak within 2× the average) and the low rung's
// startup cost; at half these caps film grain lost 8–14 VMAF (bench_test.go).
type rungRate struct{ crf, maxrate int } // maxrate: kbit/s; the VBV buffer holds 2 s of it

// rates by rung class (2160, 1440, 1080, 720, ≤480) per media.Video profile.
var rates = map[string][5]rungRate{
	media.VideoLive:      {{23, 32000}, {23, 18000}, {23, 12000}, {22, 7000}, {21, 3000}},
	media.VideoAnimation: {{21, 24000}, {21, 14000}, {21, 8000}, {21, 5000}, {20, 2400}},
}

// av1CRF is SVT-AV1's CRF by rung class; at 1080p CRF 32 matches x264's
// CRF 23 VMAF at 36% of its bits.
var av1CRF = map[string][5]int{
	media.VideoLive:      {34, 33, 32, 31, 30},
	media.VideoAnimation: {30, 30, 30, 30, 29},
}

// modernCap scales H.264's bitrate caps for HEVC and AV1, which reach the
// same VMAF at half the bits or less.
const modernCap = 0.6

// rate is the rung's capped CRF in codec c: H.264's table; HEVC one CRF
// below it (x265 CRF 22 ≈ x264 CRF 23 at 1080p); AV1 av1CRF.
func (r rung) rate(c media.Codec) rungRate {
	i := 4
	switch {
	case r.n > 1440:
		i = 0
	case r.n > 1080:
		i = 1
	case r.n > 720:
		i = 2
	case r.n > 480:
		i = 3
	}
	rt := rates[r.profile][i]
	switch c {
	case media.CodecHEVC:
		return rungRate{rt.crf - 1, int(float64(rt.maxrate) * modernCap)}
	case media.CodecAV1:
		return rungRate{av1CRF[r.profile][i], int(float64(rt.maxrate) * modernCap)}
	}
	return rt
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

// nativeBelow: a source whose short side is below it and not a rung also
// gets a rung at its own short side, so SD and 720p uploads keep their
// resolution.
const nativeBelow = 1080

// rungs is the ladder for a w×h display at fps, largest first: rungs above
// the short side are dropped, a source below nativeBelow gets its own short
// side as a rung, and a rung whose capped frame repeats the next one's is
// dropped.
func rungs(ladder []int, w, h int, fps float64, profile string) []rung {
	short := min(w, h)
	var ns []int
	for _, n := range ladder {
		if n <= short {
			ns = append(ns, n)
		}
	}
	if own := max(2, short-short%2); short < nativeBelow && !slices.Contains(ns, own) {
		ns = append(ns, own)
	}
	slices.SortFunc(ns, func(a, b int) int { return b - a })
	var out []rung
	for i, n := range ns {
		rw, rh := frame(w, h, n)
		if i+1 < len(ns) {
			if nw, nh := frame(w, h, ns[i+1]); nw == rw && nh == rh {
				continue
			}
		}
		out = append(out, rung{n: n, w: rw, h: rh, level: level(rw, rh, fps), profile: profile})
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
