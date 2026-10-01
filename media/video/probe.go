package video

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/open-rails/contentkit/media"
)

type probeResult struct {
	// Packets and MaxFPS are plan-time facts kept with the encode runs: the
	// video stream's real packets and the upload's output rate cap.
	Packets *packetScan   `json:"packets,omitempty"`
	MaxFPS  float64       `json:"max_fps,omitempty"`
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration   string `json:"duration"`
		StartTime  string `json:"start_time"`
		FormatName string `json:"format_name"`
		Tags       struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
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
	Duration   string `json:"duration"`
	AvgRate    string `json:"avg_frame_rate"`
	Rate       string `json:"r_frame_rate"`
	Tags       struct {
		Language string `json:"language"`
		Title    string `json:"title"`
		Rotate   string `json:"rotate"`
		Duration string `json:"DURATION"`
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

// packetScan is a stream's packets as demuxed: their count and the span of
// their timestamps, seconds on the container's clock.
type packetScan struct {
	Count int     `json:"count"`
	Start float64 `json:"start"`
	End   float64 `json:"end"` // the last packet's end
}

// scanTimeout bounds reading a whole source's packets.
const scanTimeout = 15 * time.Minute

// scanPackets demuxes stream of src (no decoding) for its real packets:
// containers declare durations and rates, the packets are what plays.
func scanPackets(ctx context.Context, src string, stream int, opts []string) (packetScan, error) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	pr, pw := io.Pipe()
	var s packetScan
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			pts, dur, _ := strings.Cut(strings.TrimSpace(sc.Text()), ",")
			if pts == "" {
				continue
			}
			s.Count++
			t, err := strconv.ParseFloat(pts, 64)
			if err != nil || math.IsInf(t, 0) || math.IsNaN(t) {
				continue
			}
			d, _ := strconv.ParseFloat(dur, 64)
			if s.Count == 1 || t < s.Start {
				s.Start = t
			}
			s.End = max(s.End, t+max(0, d))
		}
		_, _ = io.Copy(io.Discard, pr)
		done <- sc.Err()
	}()
	args := append(append([]string{"-v", "error"}, opts...), "-select_streams", strconv.Itoa(stream),
		"-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", src)
	err := run(ctx, pw, "ffprobe", args...)
	_ = pw.Close()
	if serr := <-done; err == nil {
		err = serr
	}
	if err == nil && s.Count == 0 {
		err = errNoVideo
	}
	return s, err
}

func probe(ctx context.Context, path string) (probeResult, error) {
	return probeWith(ctx, path, sourceDemuxers)
}

func probeRemote(ctx context.Context, url string) (probeResult, error) {
	return probeOptions(ctx, url, remoteInputOptions(sourceDemuxers))
}

func probeWith(ctx context.Context, path string, demuxers []string) (probeResult, error) {
	return probeOptions(ctx, path, inputOptions(demuxers))
}

func probeOptions(ctx context.Context, path string, opts []string) (probeResult, error) {
	var p probeResult
	out, err := command(ctx, "ffprobe", append(append([]string{"-v", "error"}, opts...),
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

// plan is a probed source: its video stream, display size and tracks.
type plan struct {
	duration      float64
	video         int     // input stream index
	width, height int     // displayed: square pixels, rotation applied
	fps           float64 // output rate: the source's, at most maxFPS and MaxFPS, at most its real rate
	limitFPS      bool    // the source's declared rate is faster than fps
	realFPS       float64 // packets per second, when scanned
	tileW, tileH  int     // sprite tile
	audio, subs   []track
	stream        probeStream // the video stream, for passthrough
	formatName    string
	start         float64 // the container's start time
}

var errNoVideo = errors.New("source has no video stream")

// minSourceFPS is the lowest real frame rate a scanned video may average:
// sparser sources leave chunks with nothing to encode.
const minSourceFPS = 1

func newPlan(p probeResult) (plan, error) {
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
			if d := selectedVideoEnd(s, pl.start, p.Format.FormatName); d > 0 {
				pl.duration = d
			}
			if n, d, ok := ratio(s.SAR); ok && n != d {
				pl.width = max(2, int(math.Round(float64(s.Width)*float64(n)/float64(d))))
			}
			r := rate(s)
			out := min(r, maxFPS)
			if p.MaxFPS > 0 {
				out = min(out, p.MaxFPS)
			}
			if pk := p.Packets; pk != nil && pk.Count > 0 && pk.End > pk.Start {
				// The container's word for duration and rate is not trusted:
				// a sparse source averaging under its declared rate is
				// encoded at its real rate, one whose last frame ends well
				// before its declared duration ends there.
				span := pk.End - pk.Start
				pl.realFPS = float64(pk.Count) / span
				if r*span > 1.5*float64(pk.Count) {
					out = min(out, pl.realFPS)
				}
				if end := pk.End - pl.start; pl.duration > end+max(1, end/100) {
					pl.duration = end
				}
			}
			pl.fps, pl.limitFPS = out, r > out
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
	if pl.realFPS > 0 && pl.realFPS < minSourceFPS {
		return pl, &media.ImageError{Code: media.CodeVideoOverBudget, Message: fmt.Sprintf(
			"the video has %d frames over %.0f seconds; a video must average at least %d frame a second", p.Packets.Count, pl.duration, minSourceFPS)}
	}
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

// ladder is an HLS preset's rungs for the source, largest first; a source
// outside its aspect bounds fails.
func (pl plan) ladder(h *media.HLS) ([]rung, error) {
	lo, hi := h.Aspects()
	if err := checkAspect(pl.width, pl.height, lo, hi); err != nil {
		return nil, err
	}
	rs := rungs(h.Rungs(), pl.width, pl.height, pl.fps, h.Profile)
	if len(rs) == 0 {
		return nil, errors.New("video has no applicable rendition rung")
	}
	return rs, nil
}

// rung is an MP4 preset's rung; false above the source (no upscaling).
func (pl plan) rung(m *media.MP4) (rung, bool) {
	if m.Rung > min(pl.width, pl.height) {
		return rung{}, false
	}
	w, h := frame(pl.width, pl.height, m.Rung)
	return rung{n: m.Rung, w: w, h: h, level: level(w, h, pl.fps), profile: m.Profile}, true
}

// defaultAudio is the source's default audio track, if any.
func (pl plan) defaultAudio() (track, int, bool) {
	for i, a := range pl.audio {
		if a.def {
			return a, i, true
		}
	}
	return track{}, -1, false
}

// The container may outlast the selected video because it also carries audio.
// Some Matroska probes report the container duration on streams, while the
// DURATION tag records the selected stream's actual length.
func selectedVideoEnd(s probeStream, containerStart float64, format string) float64 {
	d, _ := strconv.ParseFloat(s.Duration, 64)
	if strings.HasPrefix(format, "matroska,") && s.Tags.Duration != "" {
		parts := strings.Split(s.Tags.Duration, ":")
		if len(parts) == 3 {
			hours, e1 := strconv.ParseFloat(parts[0], 64)
			minutes, e2 := strconv.ParseFloat(parts[1], 64)
			seconds, e3 := strconv.ParseFloat(parts[2], 64)
			if e1 == nil && e2 == nil && e3 == nil && hours >= 0 && minutes >= 0 && minutes < 60 && seconds >= 0 && seconds < 60 {
				d = hours*3600 + minutes*60 + seconds
			}
		}
	}
	if d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return 0
	}
	start, err := strconv.ParseFloat(s.StartTime, 64)
	if err != nil || math.IsInf(start, 0) || math.IsNaN(start) {
		start = containerStart
	}
	end := start - containerStart + d
	if end <= 0 || math.IsInf(end, 0) || math.IsNaN(end) {
		return 0
	}
	return end
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
