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
//	CONTENTKIT_BENCH_NVENC_CQ_OFFSET  NVENC H.264 CQ over the rung CRF
//	CONTENTKIT_BENCH_PROFILE   media.Video.Profile
//	CONTENTKIT_BENCH_CAP_SCALE multiplies the rung bitrate caps
func benchKnobs(cfg *video.Config) func() {
	cfg.Encoder = os.Getenv("CONTENTKIT_BENCH_ENCODER")
	cfg.Preset, cfg.TopPreset = os.Getenv("CONTENTKIT_BENCH_PRESET"), os.Getenv("CONTENTKIT_BENCH_TOP_PRESET")
	undo := func() {}
	if f, err := strconv.ParseFloat(os.Getenv("CONTENTKIT_BENCH_CAP_SCALE"), 64); err == nil {
		u0 := undo
		u := video.SetCapScale(f)
		undo = func() { u(); u0() }
	}
	if o, err := strconv.Atoi(os.Getenv("CONTENTKIT_BENCH_NVENC_CQ_OFFSET")); err == nil {
		u := video.SetNVENCCQOffset(media.CodecH264, o)
		return func() { u(); undo() }
	}
	return undo
}

func benchVideo() media.Video { return media.Video{Profile: os.Getenv("CONTENTKIT_BENCH_PROFILE")} }

func knobsNote() string {
	var out []string
	for _, k := range []string{"ENCODER", "PRESET", "TOP_PRESET", "EQUAL_THREADS", "NVENC_CQ_OFFSET", "PROFILE", "CAP_SCALE"} {
		if v := os.Getenv("CONTENTKIT_BENCH_" + k); v != "" {
			out = append(out, strings.ToLower(k)+"="+v)
		}
	}
	return strings.Join(out, " ")
}

func stageOf(p media.EncodeProgress) int { return p.Stage }
