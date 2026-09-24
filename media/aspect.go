package media

import (
	"fmt"
	"strconv"
	"strings"
)

// Aspect is a width:height ratio in lowest terms, written "W:H" ("3:1",
// "9:16"). The zero value is AspectNative: the source's own shape.
type Aspect struct{ W, H int }

// maxAspectTerm bounds each term so products with widths stay exact.
const maxAspectTerm = 10000

var (
	AspectNative = Aspect{}
	Aspect1x1    = Aspect{1, 1}
	Aspect3x1    = Aspect{3, 1}
	Aspect4x5    = Aspect{4, 5}
	Aspect16x9   = Aspect{16, 9}
	Aspect9x16   = Aspect{9, 16}
	Aspect21x9   = Aspect{7, 3} // "21:9" reduces to 7:3
)

// Ratio parses "W:H" and panics on an invalid one (for constants in code).
func Ratio(s string) Aspect {
	a, err := ParseAspect(s)
	if err != nil {
		panic(err)
	}
	return a
}

// ParseAspect parses "W:H" with positive integer terms up to 10000 and
// reduces it ("6:2" is "3:1"); "" and "native" are AspectNative.
func ParseAspect(s string) (Aspect, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "native" {
		return AspectNative, nil
	}
	ws, hs, ok := strings.Cut(s, ":")
	w, werr := strconv.Atoi(ws)
	h, herr := strconv.Atoi(hs)
	if !ok || werr != nil || herr != nil || w <= 0 || h <= 0 || w > maxAspectTerm || h > maxAspectTerm {
		return Aspect{}, fmt.Errorf("media: invalid aspect %q (want W:H with positive integers up to %d)", s, maxAspectTerm)
	}
	g := gcd(w, h)
	return Aspect{w / g, h / g}, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Native reports the source's own shape.
func (a Aspect) Native() bool { return a.W == 0 || a.H == 0 }

// Valid reports a native or reduced, bounded ratio.
func (a Aspect) Valid() bool {
	if a == AspectNative {
		return true
	}
	return a.W > 0 && a.H > 0 && a.W <= maxAspectTerm && a.H <= maxAspectTerm && gcd(a.W, a.H) == 1
}

func (a Aspect) String() string {
	if a.Native() {
		return "native"
	}
	return strconv.Itoa(a.W) + ":" + strconv.Itoa(a.H)
}

// Height is width's height at a, rounded half up, at least 1 (0 when native).
func (a Aspect) Height(width int) int { return a.scale(width, a.H, a.W) }

// Width is height's width at a, rounded half up, at least 1 (0 when native).
func (a Aspect) Width(height int) int { return a.scale(height, a.W, a.H) }

func (a Aspect) scale(v, num, den int) int {
	if a.Native() {
		return 0
	}
	return max(1, (2*v*num+den)/(2*den))
}

// Rotated is a turned a quarter (90 or 270 degrees).
func (a Aspect) Rotated() Aspect { return Aspect{a.H, a.W} }

// AspectOf is the ratio of a w×h size, reduced (native for an empty size).
func AspectOf(w, h int) Aspect {
	if w <= 0 || h <= 0 {
		return AspectNative
	}
	g := gcd(w, h)
	return Aspect{w / g, h / g}
}

// MarshalText writes "W:H" (native: empty), so JSON and config carry the string.
func (a Aspect) MarshalText() ([]byte, error) {
	if a.Native() {
		return nil, nil
	}
	return []byte(a.String()), nil
}

func (a *Aspect) UnmarshalText(b []byte) error {
	v, err := ParseAspect(string(b))
	if err != nil {
		return err
	}
	*a = v
	return nil
}
