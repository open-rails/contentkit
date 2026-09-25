package media

import (
	"path"
	"slices"
	"strings"
)

// SubtitleTypes are the uploaded subtitle sidecars' types: WebVTT, SubRip
// and SubStation Alpha (SSA/ASS). A kind listing one needs Video. The media
// worker converts each to WebVTT (the file's SubtitleVariant) and its video
// lists it as a subtitle track after the source's own, without re-encoding.
var SubtitleTypes = []string{"text/vtt", "application/x-subrip", "text/x-ssa", "text/x-ass"}

// SubtitleType is the subtitle type of a file name's extension, or "".
func SubtitleType(name string) string {
	return map[string]string{".vtt": "text/vtt", ".srt": "application/x-subrip", ".ssa": "text/x-ssa", ".ass": "text/x-ass"}[strings.ToLower(path.Ext(name))]
}

// SubtitleVariant is a subtitle file's WebVTT variant (read API ?variant=vtt).
const SubtitleVariant = "vtt"

// Subtitle sidecar meta (Op.Meta on insert or replace). To change it,
// remove and insert the file in one commit.
const (
	MetaLang    = "lang"    // BCP 47 (normalized by processing); also picks a non-UTF-8 file's charset
	MetaLabel   = "label"   // track name; default the language's English name
	MetaForced  = "forced"  // bool: forced-narrative track
	MetaFor     = "for"     // the video file it subtitles; default the manifest's first video
	MetaCharset = "charset" // IANA charset overriding detection, e.g. "shift_jis"
)

func isSubtitleType(t string) bool { return slices.Contains(SubtitleTypes, t) }

// subtitleTrack is one subtitle track of a video file: File is the manifest
// file whose blob it signs (the video for its own tracks, else the sidecar).
type subtitleTrack struct {
	Subtitle
	File int
}

// subtitles are video file i's tracks: its source's, then the converted
// sidecars this viewer may read that name it (or, without MetaFor, the
// first video), in manifest order. A sidecar's id is its file name; one
// whose name is a source track's id is left out.
func (g *Grant) subtitles(i int, h *HLS) []subtitleTrack {
	var out []subtitleTrack
	for _, s := range h.Subs {
		out = append(out, subtitleTrack{s, i})
	}
	m := g.Manifest
	first, _ := VideoFile(m, "")
	for j, f := range m.Files {
		v, ok := f.Variants[SubtitleVariant]
		if !isSubtitleType(f.Type) || !ok || !g.Allowed(j) || f.Failed() != nil {
			continue
		}
		target, _ := f.Meta[MetaFor].(string)
		if target == "" {
			target = first.Name
		}
		if target != m.Files[i].Name || slices.ContainsFunc(h.Subs, func(s Subtitle) bool { return s.ID == f.Name }) {
			continue
		}
		lang, _ := f.Meta[MetaLang].(string)
		label, _ := f.Meta[MetaLabel].(string)
		forced, _ := f.Meta[MetaForced].(bool)
		out = append(out, subtitleTrack{Subtitle{ID: f.Name, Lang: lang, Label: label, Forced: forced, Blob: v.Blob}, j})
	}
	return out
}
