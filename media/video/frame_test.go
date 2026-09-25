package video

import (
	"errors"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestFrame(t *testing.T) {
	for _, c := range []struct{ w, h, n, ww, wh int }{
		{1920, 1080, 1080, 1920, 1080},
		{1920, 1080, 720, 1280, 720},
		{1080, 1920, 720, 720, 1280},    // vertical: n is the width
		{3840, 2160, 2160, 3840, 2160},  // 4K UHD
		{7680, 4320, 2160, 3840, 2160},  // 8K downscaled to the area cap
		{7680, 4320, 4320, 3840, 2160},  // a 4320 rung is capped too
		{4096, 2160, 2160, 3966, 2090},  // DCI 4K: area cap
		{5040, 2160, 2160, 4096, 1756},  // 21:9: 4096 side cap
		{2160, 5040, 2160, 1756, 4096},  // 9:21
		{10080, 4320, 2160, 4096, 1756}, // 21:9 8K
		{2560, 1080, 1080, 2560, 1080},  // 64:27 below the caps
		{3440, 1440, 1440, 3440, 1440},  // ultrawide QHD
		{1281, 721, 720, 1280, 720},     // odd source
		{853, 479, 478, 852, 478},       // never above the source
		{2160, 3840, 1440, 1440, 2560},  // vertical 4K, lower rung
		{15360, 8640, 2160, 3840, 2160}, // 16K
		{4320, 10080, 2160, 1756, 4096}, // 9:21 8K
		{3840, 1646, 1646, 3840, 1646},  // wide, fits exactly
		{4000, 3000, 3000, 3324, 2494},  // 4:3 area cap
		{3000, 4000, 1080, 1080, 1440},  // 3:4 vertical
		{8192, 4320, 2160, 3966, 2090},  // DCI 8K
	} {
		w, h := frame(c.w, c.h, c.n)
		if w != c.ww || h != c.wh {
			t.Errorf("frame(%dx%d, %d) = %dx%d, want %dx%d", c.w, c.h, c.n, w, h, c.ww, c.wh)
		}
		if w%2 != 0 || h%2 != 0 || max(w, h) > maxSide || w*h > maxArea {
			t.Errorf("frame(%dx%d, %d) = %dx%d breaks the caps", c.w, c.h, c.n, w, h)
		}
	}
}

func TestRungs(t *testing.T) {
	for _, c := range []struct {
		name   string
		ladder []int
		w, h   int
		fps    float64
		want   []rung
	}{
		{"4K 16:9", media.DefaultLadder, 3840, 2160, 30, []rung{
			{n: 2160, w: 3840, h: 2160, level: "5.1"}, {n: 1440, w: 2560, h: 1440, level: "5.0"}, {n: 1080, w: 1920, h: 1080, level: ""}, {n: 720, w: 1280, h: 720, level: ""}, {n: 480, w: 854, h: 480, level: ""}}},
		{"4K 60 fps", []int{2160}, 3840, 2160, 60, []rung{{n: 2160, w: 3840, h: 2160, level: "5.2"}}},
		{"9:21 4K", media.DefaultLadder, 2160, 5040, 30, []rung{
			{n: 2160, w: 1756, h: 4096, level: "5.1"}, {n: 1440, w: 1440, h: 3360, level: "5.0"}, {n: 1080, w: 1080, h: 2520, level: "5.0"}, {n: 720, w: 720, h: 1680, level: ""}, {n: 480, w: 480, h: 1120, level: ""}}},
		{"21:9 1080", media.DefaultLadder, 2520, 1080, 24, []rung{
			{n: 1080, w: 2520, h: 1080, level: "5.0"}, {n: 720, w: 1680, h: 720, level: ""}, {n: 480, w: 1120, h: 480, level: ""}}},
		{"8K", []int{4320, 2160, 1080}, 7680, 4320, 30, []rung{{n: 2160, w: 3840, h: 2160, level: "5.1"}, {n: 1080, w: 1920, h: 1080, level: ""}}},
		{"below the ladder", media.DefaultLadder, 427, 241, 30, []rung{{n: 240, w: 426, h: 240, level: ""}}},
		{"vertical below", media.DefaultLadder, 361, 640, 30, []rung{{n: 360, w: 360, h: 638, level: ""}}},
	} {
		got := rungs(c.ladder, c.w, c.h, c.fps, media.VideoLive)
		for i := range got {
			got[i].crf, got[i].maxrate = 0, 0
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRungRates(t *testing.T) {
	var got []rungRate
	for _, r := range rungs([]int{2160, 1440, 1080, 720, 480, 360}, 3840, 2160, 30, media.VideoLive) {
		got = append(got, rungRate{r.crf, r.maxrate})
	}
	want := []rungRate{{23, 32000}, {23, 18000}, {23, 12000}, {22, 7000}, {21, 3000}, {21, 3000}}
	if !slices.Equal(got, want) {
		t.Fatalf("live rates %v, want %v", got, want)
	}
	if r := rungs([]int{1080}, 1920, 1080, 24, media.VideoAnimation)[0]; r.crf != 21 || r.maxrate != 8000 {
		t.Fatalf("animation 1080 %+v", r)
	}
}

func TestLevel(t *testing.T) {
	for _, c := range []struct {
		w, h int
		fps  float64
		want string
	}{
		{1920, 1080, 60, ""}, {2560, 1440, 30, "5.0"}, {2560, 1440, 60, "5.1"}, {2520, 1080, 30, "5.0"}, {3840, 2160, 30, "5.1"}, {3840, 2160, 60, "5.2"},
		{4096, 1756, 30, "5.1"}, {1756, 4096, 60, "5.2"}, {3324, 2494, 24, "5.1"}, {3840, 2160, 120, "5.2"},
	} {
		if got := level(c.w, c.h, c.fps); got != c.want {
			t.Errorf("level(%dx%d@%g) = %q, want %q", c.w, c.h, c.fps, got, c.want)
		}
	}
}

func TestAspectAndTile(t *testing.T) {
	lo, hi := (*media.Video)(nil).Aspects()
	for _, c := range []struct {
		w, h int
		ok   bool
	}{
		{1920, 1080, true}, {2520, 1080, true}, {1080, 2520, true}, {5040, 2160, true},
		{2560, 1080, true}, {1080, 2560, true}, {3440, 1440, true}, {1920, 804, true}, {804, 1920, true}, {2410, 1000, true},
		{2500, 1000, false}, {1000, 2500, false}, {3000, 1000, false}, {1000, 3000, false},
	} {
		err := checkAspect(c.w, c.h, lo, hi)
		if (err == nil) != c.ok || err != nil && !errors.Is(err, ErrAspect) {
			t.Errorf("checkAspect(%dx%d) = %v", c.w, c.h, err)
		}
	}
	for _, c := range []struct{ w, h, tw, th int }{
		{1920, 1080, 160, 90}, {1080, 1920, 90, 160}, {2520, 1080, 210, 90}, {1080, 2520, 90, 210}, {1000, 1000, 90, 90},
	} {
		if tw, th := tile(c.w, c.h); tw != c.tw || th != c.th {
			t.Errorf("tile(%dx%d) = %dx%d, want %dx%d", c.w, c.h, tw, th, c.tw, c.th)
		}
	}
}
