package media

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Playlist content types.
const (
	HLSContentType = "application/vnd.apple.mpegurl"
	VTTContentType = "text/vtt; charset=utf-8"
)

const (
	audioGroup = "audio"
	subsGroup  = "subs"
)

// MasterOptions filter a master playlist's renditions by id or BCP 47 tag;
// nil keeps every track, an empty non-nil slice none.
type MasterOptions struct {
	Audio, Subs []string
}

// Playlist URIs relative to the master playlist, which is served at
// ".../hls/{file}/master.m3u8".
func videoURI(rung int) string  { return "video/" + strconv.Itoa(rung) + ".m3u8" }
func audioURI(id string) string { return "audio/" + id + ".m3u8" }
func subsURI(id string) string  { return "subs/" + id + ".m3u8" }

// hlsFile returns the index and ladder of a file this viewer may play.
func (g *Grant) hlsFile(name string) (int, *HLS, error) {
	i := g.Manifest.File(name)
	if !g.Allowed(i) || g.Manifest.Files[i].HLS == nil || len(g.Manifest.Files[i].HLS.Video) == 0 {
		return 0, nil, ErrNotAllowed
	}
	return i, g.Manifest.Files[i].HLS, nil
}

// MasterPlaylist is the multivariant playlist of file: one variant per video
// rendition, with alternative audio and subtitle groups.
func (g *Grant) MasterPlaylist(file string, o MasterOptions) ([]byte, error) {
	_, h, err := g.hlsFile(file)
	if err != nil {
		return nil, err
	}
	audio := pick(h.Audio, o.Audio, func(a AudioTrack) (string, string) { return a.ID, a.Lang })
	subs := pick(h.Subs, o.Subs, func(s Subtitle) (string, string) { return s.ID, s.Lang })

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	audioBW, audioCodecs := 0, []string{}
	def := slices.IndexFunc(audio, func(a AudioTrack) bool { return a.Default })
	names := map[string]bool{}
	for i, a := range audio {
		audioBW = max(audioBW, a.Bandwidth)
		c := orDefaultString(a.Codecs, "mp4a.40.2")
		if !slices.Contains(audioCodecs, c) {
			audioCodecs = append(audioCodecs, c)
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=%q,NAME=%q%s,DEFAULT=%s,AUTOSELECT=YES,URI=%q\n",
			audioGroup, uniqueName(names, a.Label, a.Lang, a.ID), language(a.Lang), yesNo(i == max(def, 0)), audioURI(a.ID))
	}
	names = map[string]bool{}
	for _, s := range subs {
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q%s,DEFAULT=NO,AUTOSELECT=YES,FORCED=%s,URI=%q\n",
			subsGroup, uniqueName(names, s.Label, s.Lang, s.ID), language(s.Lang), yesNo(s.Forced), subsURI(s.ID))
	}
	for _, v := range variantOrder(h.Video) {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Bandwidth+audioBW)
		if v.Average > 0 {
			fmt.Fprintf(&b, ",AVERAGE-BANDWIDTH=%d", v.Average+audioBW)
		}
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		fmt.Fprintf(&b, ",CODECS=%q", strings.Join(append([]string{v.Codecs}, audioCodecs...), ","))
		if len(audio) > 0 {
			fmt.Fprintf(&b, ",AUDIO=%q", audioGroup)
		}
		if len(subs) > 0 {
			fmt.Fprintf(&b, ",SUBTITLES=%q", subsGroup)
		}
		b.WriteString(",CLOSED-CAPTIONS=NONE\n" + videoURI(v.Rung) + "\n")
	}
	return []byte(b.String()), nil
}

// StartRung is the short side of the variant listed first: native HLS
// players (Safari, iOS) start there before measuring.
const StartRung = 1080

// variantOrder lists the highest rendition up to StartRung first (else the
// smallest), then the rest by descending bandwidth.
func variantOrder(video []Rendition) []Rendition {
	out := slices.Clone(video)
	slices.SortStableFunc(out, func(a, b Rendition) int { return b.Bandwidth - a.Bandwidth })
	start := len(out) - 1
	for i, v := range out {
		if shortSide(v) <= StartRung {
			start = i
			break
		}
	}
	first := out[start]
	return append([]Rendition{first}, slices.Delete(out, start, start+1)...)
}

func shortSide(v Rendition) int {
	if v.Width > 0 && v.Height > 0 {
		return min(v.Width, v.Height)
	}
	return v.Rung
}

// VideoPlaylist is the byte-range media playlist of one video rung.
func (g *Grant) VideoPlaylist(file string, rung int) ([]byte, error) {
	i, h, err := g.hlsFile(file)
	if err != nil {
		return nil, err
	}
	for _, v := range h.Video {
		if v.Rung == rung {
			return g.mediaPlaylist(i, v.Blob, v.Segments)
		}
	}
	return nil, ErrNotAllowed
}

// AudioPlaylist is the byte-range media playlist of one audio track.
func (g *Grant) AudioPlaylist(file, id string) ([]byte, error) {
	i, h, err := g.hlsFile(file)
	if err != nil {
		return nil, err
	}
	for _, a := range h.Audio {
		if a.ID == id {
			return g.mediaPlaylist(i, a.Blob, a.Segments)
		}
	}
	return nil, ErrNotAllowed
}

// SubtitlePlaylist is a one-segment playlist over the whole WebVTT blob.
func (g *Grant) SubtitlePlaylist(file, id string) ([]byte, error) {
	i, h, err := g.hlsFile(file)
	if err != nil {
		return nil, err
	}
	for _, s := range h.Subs {
		if s.ID != id {
			continue
		}
		u, err := g.URL(i, s.Blob)
		if err != nil {
			return nil, err
		}
		d := duration(g.Manifest.Files[i], h)
		return fmt.Appendf(nil, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%s,\n%s\n#EXT-X-ENDLIST\n",
			max(1, int(math.Ceil(d))), seconds(d), u), nil
	}
	return nil, ErrNotAllowed
}

// SpriteVTT is the seek-preview track: one cue per sprite tile, each
// pointing at its tile with a #xywh fragment.
func (g *Grant) SpriteVTT(file string) ([]byte, error) {
	i, h, err := g.hlsFile(file)
	if err != nil {
		return nil, err
	}
	s := h.Sprite
	if s == nil || s.Cols <= 0 || s.Rows <= 0 || s.Interval <= 0 {
		return nil, ErrNotAllowed
	}
	u, err := g.URL(i, s.Blob)
	if err != nil {
		return nil, err
	}
	d := duration(g.Manifest.Files[i], h)
	n := s.Cols * s.Rows
	if d > 0 {
		n = min(n, int(math.Ceil(d/s.Interval)))
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for t := range n {
		start, end := float64(t)*s.Interval, float64(t+1)*s.Interval
		if d > 0 {
			end = min(end, d)
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s#xywh=%d,%d,%d,%d\n", vttTime(start), vttTime(end), u,
			t%s.Cols*s.Width, t/s.Cols*s.Height, s.Width, s.Height)
	}
	return []byte(b.String()), nil
}

// mediaPlaylist lists segs as byte ranges of one fMP4 blob whose init
// segment is bytes [0, segs[0].Offset).
func (g *Grant) mediaPlaylist(i int, blob string, segs []Segment) ([]byte, error) {
	if len(segs) == 0 || segs[0].Offset <= 0 {
		return nil, fmt.Errorf("media: rendition %s has no init segment or segments", blob)
	}
	u, err := g.URL(i, blob)
	if err != nil {
		return nil, err
	}
	target := 1
	for _, s := range segs {
		target = max(target, int(math.Round(s.Seconds)))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n", target)
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q,BYTERANGE=\"%d@0\"\n", u, segs[0].Offset)
	for _, s := range segs {
		fmt.Fprintf(&b, "#EXTINF:%s,\n#EXT-X-BYTERANGE:%d@%d\n%s\n", seconds(s.Seconds), s.Length, s.Offset, u)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), nil
}

// duration is the ladder's length: its first rendition's segments, else meta.
func duration(f File, h *HLS) float64 {
	var d float64
	for _, s := range h.Video[0].Segments {
		d += s.Seconds
	}
	if d > 0 {
		return d
	}
	return metaFloat(f.Meta, "duration")
}

func pick[T any](tracks []T, want []string, key func(T) (id, lang string)) []T {
	if want == nil {
		return tracks
	}
	var out []T
	for _, t := range tracks {
		id, lang := key(t)
		if slices.Contains(want, id) || (lang != "" && slices.Contains(want, lang)) {
			out = append(out, t)
		}
	}
	return out
}

// uniqueName is a NAME attribute unique within its group.
func uniqueName(seen map[string]bool, label, lang, id string) string {
	n := quotable(orDefaultString(orDefaultString(label, lang), id))
	if seen[n] {
		n += " (" + quotable(id) + ")"
	}
	seen[n] = true
	return n
}

func language(lang string) string {
	if lang = quotable(lang); lang == "" {
		return ""
	}
	return ",LANGUAGE=\"" + lang + "\""
}

// quotable strips what an HLS quoted-string may not hold; %q then only
// quotes, as the rest is printable.
func quotable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

func orDefaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

func seconds(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }

func vttTime(s float64) string {
	ms := int64(math.Round(s * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
