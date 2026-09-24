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
//	CONTENTKIT_BENCH_ENCODER   Config.Encoder
//	CONTENTKIT_BENCH_PRESET, CONTENTKIT_BENCH_TOP_PRESET  Config.Preset, TopPreset
//	CONTENTKIT_BENCH_EQUAL_THREADS  every rung gets all threads
//	CONTENTKIT_BENCH_NVENC_CQ_OFFSET  NVENC CQ over the rung CRF
//	CONTENTKIT_BENCH_PROFILE   media.Video.Profile
func benchKnobs(cfg *video.Config) func() {
	cfg.Encoder = os.Getenv("CONTENTKIT_BENCH_ENCODER")
	cfg.Preset, cfg.TopPreset = os.Getenv("CONTENTKIT_BENCH_PRESET"), os.Getenv("CONTENTKIT_BENCH_TOP_PRESET")
	undo := video.SetEqualRungThreads(os.Getenv("CONTENTKIT_BENCH_EQUAL_THREADS") != "")
	if o, err := strconv.Atoi(os.Getenv("CONTENTKIT_BENCH_NVENC_CQ_OFFSET")); err == nil {
		u := video.SetNVENCCQOffset(o)
		return func() { u(); undo() }
	}
	return undo
}

func benchVideo() media.Video { return media.Video{Profile: os.Getenv("CONTENTKIT_BENCH_PROFILE")} }

func knobsNote() string {
	var out []string
	for _, k := range []string{"ENCODER", "PRESET", "TOP_PRESET", "EQUAL_THREADS", "NVENC_CQ_OFFSET", "PROFILE"} {
		if v := os.Getenv("CONTENTKIT_BENCH_" + k); v != "" {
			out = append(out, strings.ToLower(k)+"="+v)
		}
	}
	return strings.Join(out, " ")
}

func stageOf(p media.EncodeProgress) int { return p.Stage }
