package video

import (
	"math"
	"strconv"
	"testing"

	"github.com/open-rails/contentkit/media"
)

// probeOf is a probed 1-stream video declaring rate and duration, with
// packets real packets spread over it (none: not scanned).
func probeOf(w, h int, rate string, duration float64, packets int) probeResult {
	pr := probeResult{Streams: []probeStream{{CodecType: "video", CodecName: "h264", Width: w, Height: h, AvgRate: rate, Rate: rate}}}
	pr.Format.Duration = strconv.FormatFloat(duration, 'f', 3, 64)
	if packets > 0 {
		pr.Packets = &packetScan{Count: packets, End: duration}
	}
	return pr
}

// The Hentai0 video kind's encode presets in the default worker's codecs.
var (
	videoPresets = []*media.Private{{Name: "hls", HLS: &media.HLS{}}, {Name: "mp4-1080", MP4: media.Rung(1080)}, {Name: "mp4-480", MP4: media.Rung(480)}}
	twoCodecs    = []media.Codec{media.CodecAV1, media.CodecH264}
)

func refusal(t *testing.T, err error) string {
	t.Helper()
	ie := media.AsImageError(err)
	if ie == nil {
		t.Fatalf("not a refusal: %v", err)
	}
	return ie.Code
}

// The audit's sources (H4): 80,000 s of 4K at 60 fps. Densely filled it
// runs past MaxSeconds; as built (3 real frames) it is refused as sparse
// before its declared length is believed. A 2 h 30 fps source plans.
func TestAuditPlanIsRefused(t *testing.T) {
	l := (*media.VideoLimits)(nil).Limits()
	pl, err := newPlan(probeOf(3840, 2160, "60/1", 80000.017, 4_800_001))
	if err != nil {
		t.Fatal(err)
	}
	if c := refusal(t, checkLimits(pl, videoPresets, twoCodecs, l)); c != media.CodeVideoTooLong {
		t.Fatalf("dense 80,000 s: %s", c)
	}
	if _, err := newPlan(probeOf(3840, 2160, "60/1", 80000.017, 3)); refusal(t, err) != media.CodeVideoOverBudget {
		t.Fatalf("sparse 80,000 s: %v", err)
	}
	pl, err = newPlan(probeOf(1920, 1080, "30/1", 7200, 216_000))
	if err != nil || checkLimits(pl, videoPresets, twoCodecs, l) != nil || pl.fps != 30 || pl.limitFPS {
		t.Fatalf("2 h at 30 fps: %+v %v %v", pl, err, checkLimits(pl, videoPresets, twoCodecs, l))
	}
	// The default work budget admits 4 h of 4K60 in three codecs with MP4s.
	pl, err = newPlan(probeOf(3840, 2160, "60/1", 4*3600, 864_000))
	three := []media.Codec{media.CodecAV1, media.CodecHEVC, media.CodecH264}
	if err != nil || checkLimits(pl, videoPresets, three, l) != nil {
		t.Fatalf("4 h of 4K60: %v %v (work %.3g)", err, checkLimits(pl, videoPresets, three, l), work(pl, videoPresets, three))
	}
	if c := refusal(t, checkLimits(probe4K(t, 4*3600+60), videoPresets, twoCodecs, l)); c != media.CodeVideoTooLong {
		t.Fatalf("4 h 1 min: %s", c)
	}
	big, _ := newPlan(probeOf(15360, 8640, "30/1", 60, 1800))
	if c := refusal(t, checkLimits(big, videoPresets, twoCodecs, l)); c != media.CodeVideoTooLarge {
		t.Fatalf("16K: %s", c)
	}
	if pl, _ := newPlan(probeOf(8192, 4320, "30/1", 60, 1800)); checkLimits(pl, videoPresets, twoCodecs, l) != nil {
		t.Fatal("DCI 8K refused")
	}
}

func probe4K(t *testing.T, secs float64) plan {
	t.Helper()
	pl, err := newPlan(probeOf(3840, 2160, "60/1", secs, int(secs*60)))
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// Work is output pixels × output frames over every rung and codec; a
// kind's MaxWork refuses past it.
func TestWorkBudget(t *testing.T) {
	pl, err := newPlan(probeOf(1920, 1080, "30/1", 3600, 108_000))
	if err != nil {
		t.Fatal(err)
	}
	// HLS 1080 and 480 in two codecs, the 1080 and 480 MP4s once each.
	want := float64(2*(1920*1080+854*480)+1920*1080+854*480) * 3600 * 30
	if w := work(pl, videoPresets, twoCodecs); math.Abs(w-want) > 1 {
		t.Fatalf("work %.4g, want %.4g", w, want)
	}
	l := (&media.VideoLimits{MaxWork: want - 1}).Limits()
	if c := refusal(t, checkLimits(pl, videoPresets, twoCodecs, l)); c != media.CodeVideoOverBudget {
		t.Fatalf("over budget: %s", c)
	}
	if l.MaxSeconds != media.DefaultVideoLimits.MaxSeconds || checkLimits(pl, videoPresets, twoCodecs, (&media.VideoLimits{MaxWork: want}).Limits()) != nil {
		t.Fatalf("limits %+v", l)
	}
}

// A source declaring more frames than it holds is encoded at its real
// rate; MaxFPS caps the output rate; an honest source keeps its own.
func TestRealRate(t *testing.T) {
	for _, c := range []struct {
		name    string
		pr      probeResult
		maxFPS  float64
		fps     float64
		limited bool
	}{
		{"honest 60", probeOf(1280, 720, "60/1", 600, 36_000), 0, 60, false},
		{"declares 60, holds 30", probeOf(1280, 720, "60/1", 600, 18_000), 0, 30, true},
		{"VFR with a 1000/1 timebase", probeOf(1280, 720, "1000/1", 600, 14_386), 0, 14_386.0 / 600, true},
		{"MaxFPS 30", probeOf(1280, 720, "60/1", 600, 36_000), 30, 30, true},
		{"not scanned", probeOf(1280, 720, "120/1", 600, 0), 0, 60, true},
	} {
		c.pr.MaxFPS = c.maxFPS
		pl, err := newPlan(c.pr)
		if err != nil || math.Abs(pl.fps-c.fps) > 1e-9 || pl.limitFPS != c.limited {
			t.Errorf("%s: fps %g limited %v (%v), want %g %v", c.name, pl.fps, pl.limitFPS, err, c.fps, c.limited)
		}
	}
	// A last frame well before the declared end ends the video there.
	pr := probeOf(1280, 720, "30/1", 3600, 300)
	pr.Packets.End = 10
	if pl, err := newPlan(pr); err != nil || pl.duration != 10 || pl.fps != 30 {
		t.Fatalf("trailing gap: duration %g fps %g (%v)", pl.duration, pl.fps, err)
	}
}

func TestVideoLimitsValidate(t *testing.T) {
	kind := func(u media.Upload) media.Config {
		return media.Config{Namespace: "app", Kinds: []media.Kind{{Name: "video", Uploads: []media.Upload{u}}}}
	}
	video := media.Upload{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 30}
	for _, c := range []struct {
		u  media.Upload
		ok bool
	}{
		{video, true},
		{func() media.Upload {
			u := video
			u.Video = &media.VideoLimits{MaxSeconds: 600, MaxFPS: 30, MaxPixels: 1920 * 1080, MaxWork: 1e12}
			return u
		}(), true},
		{func() media.Upload { u := video; u.Video = &media.VideoLimits{MaxFPS: 120}; return u }(), false},
		{func() media.Upload { u := video; u.Video = &media.VideoLimits{MaxSeconds: -1}; return u }(), false},
		{func() media.Upload { u := video; u.Video = &media.VideoLimits{MaxWork: math.Inf(1)}; return u }(), false},
		{media.Upload{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20, Video: &media.VideoLimits{MaxSeconds: 60}}, false},
	} {
		if _, err := media.NewRegistry(kind(c.u)); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.u.Video, err)
		}
	}
}

// A source keeps at most MaxAudioTracks audio and MaxSubtitleTracks text
// subtitle tracks (what a manifest projects for its ladder), and a title's
// control characters never reach a track label.
func TestTracksAreBounded(t *testing.T) {
	pr := probeOf(640, 360, "30/1", 60, 0)
	for range 20 {
		s := probeStream{CodecType: "audio", CodecName: "aac"}
		s.Tags.Title = "\x01\x02voice "
		pr.Streams = append(pr.Streams, s)
		pr.Streams = append(pr.Streams, probeStream{CodecType: "subtitle", CodecName: "subrip"})
	}
	pl, err := newPlan(pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.audio) != media.MaxAudioTracks || len(pl.subs) != media.MaxSubtitleTracks {
		t.Fatalf("%d audio, %d subtitle tracks", len(pl.audio), len(pl.subs))
	}
	if pl.audio[0].label != "voice" {
		t.Fatalf("label %q", pl.audio[0].label)
	}
}
