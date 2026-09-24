package video

import (
	"slices"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestRungThreads(t *testing.T) {
	ladder := rungs([]int{2160, 1440, 1080, 720, 480}, 3840, 2160, 30, media.VideoLive)
	var got []int
	for i := range ladder {
		got = append(got, rungThreads(ladder, i, 32))
	}
	if want := []int{18, 8, 5, 2, 2}; !slices.Equal(got, want) {
		t.Fatalf("threads %v, want %v", got, want)
	}
	for i := range ladder {
		if n := rungThreads(ladder, i, 1); n != 1 {
			t.Fatalf("1 thread: rung %d gets %d", i, n)
		}
	}
}
