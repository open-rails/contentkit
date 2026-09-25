package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/open-rails/contentkit/media"
)

type probeResult struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration   string `json:"duration"`
		StartTime  string `json:"start_time"`
		FormatName string `json:"format_name"`
	} `json:"format"`
}

type probeStream struct {
	Index      int    `json:"index"`
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	CodecTag   string `json:"codec_tag_string"`
	Profile    string `json:"profile"`
	Level      int    `json:"level"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SAR        string `json:"sample_aspect_ratio"`
	PixFmt     string `json:"pix_fmt"`
	FieldOrder string `json:"field_order"`
	StartTime  string `json:"start_time"`
	AvgRate    string `json:"avg_frame_rate"`
	Rate       string `json:"r_frame_rate"`
	Tags       struct {
		Language string `json:"language"`
		Title    string `json:"title"`
		Rotate   string `json:"rotate"`
	} `json:"tags"`
	Disposition struct {
		Default     int `json:"default"`
		Forced      int `json:"forced"`
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	SideData []struct {
		Rotation float64 `json:"rotation"`
	} `json:"side_data_list"`
}

func probe(ctx context.Context, path string) (probeResult, error) {
	var p probeResult
	out, err := command(ctx, "ffprobe", append(append([]string{"-v", "error"}, inputOptions(sourceDemuxers)...),
		"-show_streams", "-show_format", "-of", "json", path)...)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return p, fmt.Errorf("ffprobe output: %w", err)
	}
	return p, nil
}

// WebVTT carries text only; bitmap subtitles stay in the original.
var textSubtitles = map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true}

type track struct {
	index   int // input stream index
	id      string
	lang    string // BCP 47, "" when unknown
	label   string
	def     bool
	forced  bool
	iso6392 string // MP4 language tag
}

// plan is what one encode produces from a probed source.
type plan struct {
	duration      float64
	video         int     // input stream index
	width, height int     // displayed: square pixels, rotation applied
	fps           float64 // output rate: the source's, at most maxFPS
	limitFPS      bool    // the source is faster than maxFPS
	rungs         []rung
	tileW, tileH  int // sprite tile
	audio, subs   []track
	stream        probeStream // the video stream, for passthrough
	formatName    string
	start         float64 // the container's start time
}

var errNoVideo = errors.New("source has no video stream")

func newPlan(p probeResult, v *media.Video) (plan, error) {
	var pl plan
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil || d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return pl, errors.New("source has no valid duration")
	}
	pl.duration, pl.video = d, -1
	pl.formatName = p.Format.FormatName
	pl.start, _ = strconv.ParseFloat(p.Format.StartTime, 64)
	labels := map[string]int{}
	counts := map[string]int{}
	for _, s := range p.Streams {
		if s.CodecType == "audio" || s.CodecType == "subtitle" && textSubtitles[s.CodecName] {
			counts[s.CodecType]++
		}
	}
	for _, s := range p.Streams {
		switch {
		case s.CodecType == "video" && s.Disposition.AttachedPic == 0 && pl.video < 0:
			if s.Width < 2 || s.Height < 2 {
				return pl, errors.New("invalid video dimensions")
			}
			pl.video, pl.width, pl.height, pl.stream = s.Index, s.Width, s.Height, s
			if n, d, ok := ratio(s.SAR); ok && n != d {
				pl.width = max(2, int(math.Round(float64(s.Width)*float64(n)/float64(d))))
			}
			r := rate(s)
			pl.fps, pl.limitFPS = min(r, maxFPS), r > maxFPS
			if rotated(s) {
				pl.width, pl.height = pl.height, pl.width
			}
		case s.CodecType == "audio", s.CodecType == "subtitle" && textSubtitles[s.CodecName]:
			t := track{index: s.Index, def: s.Disposition.Default == 1, forced: s.Disposition.Forced == 1}
			t.lang, t.iso6392 = normalizeLanguage(s.Tags.Language)
			t.label = trackLabel(s, t.lang, counts[s.CodecType], labels)
			if s.CodecType == "audio" {
				t.id = "a" + strconv.Itoa(len(pl.audio)+1)
				pl.audio = append(pl.audio, t)
			} else {
				t.id = "s" + strconv.Itoa(len(pl.subs)+1)
				pl.subs = append(pl.subs, t)
			}
		}
	}
	if pl.video < 0 {
		return pl, errNoVideo
	}
	lo, hi := v.Aspects()
	if err := checkAspect(pl.width, pl.height, lo, hi); err != nil {
		return pl, err
	}
	pl.rungs = rungs(v.Rungs(), pl.width, pl.height, pl.fps, v.Profile)
	pl.tileW, pl.tileH = tile(pl.width, pl.height)
	// Exactly one default audio track: the first flagged one, else the first.
	def := 0
	for i, a := range pl.audio {
		if a.def {
			def = i
			break
		}
	}
	for i := range pl.audio {
		pl.audio[i].def = i == def
	}
	return pl, nil
}

// ratio parses "n:d" or "n/d" with positive terms.
func ratio(v string) (n, d int, ok bool) {
	a, b, found := strings.Cut(v, ":")
	if !found {
		a, b, found = strings.Cut(v, "/")
	}
	n, err1 := strconv.Atoi(a)
	d, err2 := strconv.Atoi(b)
	return n, d, found && err1 == nil && err2 == nil && n > 0 && d > 0
}

// rate is the stream's frame rate; unknown is 30.
func rate(s probeStream) float64 {
	for _, v := range []string{s.AvgRate, s.Rate} {
		if n, d, ok := ratio(v); ok {
			return float64(n) / float64(d)
		}
	}
	return 30
}

func rotated(s probeStream) bool {
	r := 0.0
	if v, err := strconv.ParseFloat(s.Tags.Rotate, 64); err == nil {
		r = v
	}
	for _, sd := range s.SideData {
		if sd.Rotation != 0 {
			r = sd.Rotation
		}
	}
	return int(math.Abs(r))%180 == 90
}

func normalizeLanguage(v string) (bcp47, iso string) {
	v = strings.TrimSpace(v)
	if v == "" || v == "und" || len(v) > 35 {
		return "", "und"
	}
	tag, err := language.Parse(v)
	if err != nil {
		return "", "und"
	}
	base, _ := tag.Base()
	return tag.String(), base.ISO3()
}

func trackLabel(s probeStream, lang string, count int, seen map[string]int) string {
	label := strings.TrimSpace(s.Tags.Title)
	if label == "" && lang != "" {
		label = display.English.Tags().Name(language.Make(lang))
	}
	if label == "" {
		label = map[string]string{"audio": "Audio", "subtitle": "Subtitles"}[s.CodecType]
		if count > 1 {
			label += " " + strconv.Itoa(seen[s.CodecType+"\x00#"]+1)
			seen[s.CodecType+"\x00#"]++
		}
	}
	if len(label) > 120 {
		label = strings.ToValidUTF8(label[:120], "")
	}
	key := s.CodecType + "\x00" + label
	seen[key]++
	if n := seen[key]; n > 1 {
		return label + " " + strconv.Itoa(n)
	}
	return label
}
