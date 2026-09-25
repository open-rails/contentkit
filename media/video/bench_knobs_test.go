//go:build bench

package video_test

import (
	"os"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

// benchKnobs applies the knobs this version has:
//
//	CONTENTKIT_BENCH_PRESET, CONTENTKIT_BENCH_TOP_PRESET  Config.Preset, TopPreset
//	CONTENTKIT_BENCH_PROFILE   media.Video.Profile
//	CONTENTKIT_BENCH_CAP_SCALE multiplies the rung bitrate caps
func benchKnobs(cfg *video.Config) func() {
	cfg.Preset, cfg.TopPreset = os.Getenv("CONTENTKIT_BENCH_PRESET"), os.Getenv("CONTENTKIT_BENCH_TOP_PRESET")
	if f, err := strconv.ParseFloat(os.Getenv("CONTENTKIT_BENCH_CAP_SCALE"), 64); err == nil {
		return video.SetCapScale(f)
	}
	return func() {}
}

func benchVideo() media.Video { return media.Video{Profile: os.Getenv("CONTENTKIT_BENCH_PROFILE")} }

func knobsNote() string {
	var out []string
	for _, k := range []string{"PRESET", "TOP_PRESET", "EQUAL_THREADS", "PROFILE", "CAP_SCALE"} {
		if v := os.Getenv("CONTENTKIT_BENCH_" + k); v != "" {
			out = append(out, strings.ToLower(k)+"="+v)
		}
	}
	return strings.Join(out, " ")
}

func stageOf(p media.EncodeProgress) int { return p.Stage }
