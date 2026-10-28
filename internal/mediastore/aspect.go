package mediastore

import (
	media "github.com/open-rails/contentkit/media"
)

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// AspectOf is the ratio of a w×h size, reduced (native for an empty size).
func AspectOf(w, h int) media.Aspect {
	if w <= 0 || h <= 0 {
		return media.AspectNative
	}
	g := gcd(w, h)
	return media.Aspect{W: w / g, H: h / g}
}
