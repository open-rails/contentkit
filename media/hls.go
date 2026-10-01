package media

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path"
	"slices"
	"strings"
	"sync"
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

// StartRung is the short side of the variant listed first in each codec:
// native HLS players (Safari, iOS) start there before measuring.
const StartRung = 1080

// MasterOptions filter a master playlist's renditions by id or BCP 47 tag;
// nil keeps every track, an empty non-nil slice none.
type MasterOptions struct {
	Audio, Subs []string
}

// Playlists are served under the read API at {kind}/{id}/hls/: a ladder's
// master at {dir}master.m3u8 and its sprite at {dir}sprite.vtt, and every
// track file's media playlist at {path}.m3u8. A master lists its media
// playlists relative to its own URL.
func playlistURI(dir, filePath string) string {
	return strings.Repeat("../", strings.Count(dir, "/")) + filePath + ".m3u8"
}

// ladder lists the tracks of the HLS output directory dir this viewer may
// play, in manifest order.
func (g *Grant) ladder(dir string) (video, audio, subs []File, sprite *File) {
	for _, f := range g.Manifest.Files {
		if f.Track == nil || path.Dir(f.Path)+"/" != dir || !g.Allowed(f) {
			continue
		}
		switch f.Track.Kind {
		case TrackVideo:
			video = append(video, f)
		case TrackAudio:
			audio = append(audio, f)
		case TrackSubs:
			subs = append(subs, f)
		case TrackSprite:
			sprite = &f
		}
	}
	if len(video) > 0 {
		subs = append(subs, g.sidecars(video[0].From, subs)...)
	}
	return video, audio, subs, sprite
}

// sidecars are the converted subtitle uploads (Subtitles presets' outputs)
// this viewer may read for the video upload from: those whose upload's
// meta.for names it, or names no video. Their track id is the output path;
// one whose language-and-label duplicates a source track still lists.
func (g *Grant) sidecars(from string, have []File) []File {
	var out []File
	k := g.Item.Kind()
	for _, f := range g.Manifest.Files {
		p := k.private(f.Preset)
		if p == nil || p.Subtitles == nil || !g.Allowed(f) {
			continue
		}
		src, _ := g.Manifest.Get(f.From)
		if target, _ := src.Meta[MetaFor].(string); target != "" && target != from {
			continue
		}
		lang, _ := src.Meta[MetaLang].(string)
		label, _ := src.Meta[MetaLabel].(string)
		forced, _ := src.Meta[MetaForced].(bool)
		f.Track = &Track{Kind: TrackSubs, ID: f.Path, Lang: lang, Label: label, Forced: forced}
		if !slices.ContainsFunc(have, func(h File) bool { return h.Track.ID == f.Path }) {
			out = append(out, f)
		}
	}
	return out
}

// MasterPlaylist is the multivariant playlist of the ladder under dir: one
// variant per video track (rung and codec), with the audio and subtitle
// groups; an audio-only ladder is one audio variant. Codecs are listed in
// the ladder's order, so a player that decodes the first starts on it.
func (g *Grant) MasterPlaylist(dir string, o MasterOptions) ([]byte, error) {
	video, audio, subs, _ := g.ladder(dir)
	if len(video) == 0 && len(audio) == 0 {
		return nil, ErrNotAllowed
	}
	if len(video) == 0 {
		a := audio[0]
		return fmt.Appendf(nil, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=%q\n%s\n",
			max(a.Track.Bandwidth, 1), cmpOr(a.Track.Codecs, "mp4a.40.2"), playlistURI(dir, a.Path)), nil
	}
	audio = pickTracks(audio, o.Audio)
	subs = pickTracks(subs, o.Subs)

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	audioBW, audioCodecs := 0, []string{}
	def := slices.IndexFunc(audio, func(a File) bool { return a.Track.Default })
	names := map[string]bool{}
	for i, a := range audio {
		audioBW = max(audioBW, a.Track.Bandwidth)
		if c := cmpOr(a.Track.Codecs, "mp4a.40.2"); !slices.Contains(audioCodecs, c) {
			audioCodecs = append(audioCodecs, c)
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=%q,NAME=%q%s,DEFAULT=%s,AUTOSELECT=YES,URI=%q\n",
			audioGroup, uniqueName(names, a.Track), language(a.Track.Lang), yesNo(i == max(def, 0)), playlistURI(dir, a.Path))
	}
	names = map[string]bool{}
	for _, s := range subs {
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q%s,DEFAULT=NO,AUTOSELECT=YES,FORCED=%s,URI=%q\n",
			subsGroup, uniqueName(names, s.Track), language(s.Track.Lang), yesNo(s.Track.Forced), playlistURI(dir, s.Path))
	}
	var codecs []string
	for _, v := range video {
		if !slices.Contains(codecs, v.Track.Codec) {
			codecs = append(codecs, v.Track.Codec)
		}
	}
	for _, c := range codecs {
		for _, v := range variantOrder(slices.DeleteFunc(slices.Clone(video), func(v File) bool { return v.Track.Codec != c })) {
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Track.Bandwidth+audioBW)
			if v.Track.Average > 0 {
				fmt.Fprintf(&b, ",AVERAGE-BANDWIDTH=%d", v.Track.Average+audioBW)
			}
			if v.W > 0 && v.H > 0 {
				fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.W, v.H)
			}
			fmt.Fprintf(&b, ",CODECS=%q", strings.Join(append([]string{v.Track.Codecs}, audioCodecs...), ","))
			if len(audio) > 0 {
				fmt.Fprintf(&b, ",AUDIO=%q", audioGroup)
			}
			if len(subs) > 0 {
				fmt.Fprintf(&b, ",SUBTITLES=%q", subsGroup)
			}
			b.WriteString(",CLOSED-CAPTIONS=NONE\n" + playlistURI(dir, v.Path) + "\n")
		}
	}
	return []byte(b.String()), nil
}

// variantOrder lists the highest variant up to StartRung first (else the
// smallest), then the rest by descending bandwidth.
func variantOrder(video []File) []File {
	out := slices.Clone(video)
	slices.SortStableFunc(out, func(a, b File) int { return b.Track.Bandwidth - a.Track.Bandwidth })
	start := len(out) - 1
	for i, v := range out {
		if min(v.W, v.H) <= StartRung {
			start = i
			break
		}
	}
	first := out[start]
	return append([]File{first}, slices.Delete(out, start, start+1)...)
}

// MediaPlaylist is the playlist of the track file at filePath: a byte-range
// playlist over its blob for video and audio (from its index), or one
// segment over a whole WebVTT file.
func (g *Grant) MediaPlaylist(ctx context.Context, filePath string) ([]byte, error) {
	f, ok := g.Manifest.Get(filePath)
	if !ok || !g.Allowed(f) {
		return nil, ErrNotAllowed
	}
	u, err := g.URL(f, false)
	if err != nil {
		return nil, err
	}
	if f.Type == "text/vtt" {
		d := g.duration(f)
		return fmt.Appendf(nil, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%s,\n%s\n#EXT-X-ENDLIST\n",
			max(1, int(math.Ceil(d))), seconds(d), u), nil
	}
	if f.Track == nil || f.Track.Kind != TrackVideo && f.Track.Kind != TrackAudio {
		return nil, ErrNotAllowed
	}
	idx, err := g.r.index(ctx, g.Item, f.Track.Index)
	if err != nil {
		return nil, err
	}
	segs := idx.Segments
	if len(segs) == 0 || segs[0].Offset <= 0 {
		return nil, fmt.Errorf("media: track %s has no init segment or segments", f.Path)
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

// duration is a subtitle's: its video upload's.
func (g *Grant) duration(f File) float64 {
	src, _ := g.Manifest.Get(f.From)
	if target, _ := src.Meta[MetaFor].(string); target != "" {
		src, _ = g.Manifest.Get(target)
	}
	if src.Dur > 0 {
		return src.Dur
	}
	for _, x := range g.Manifest.Files {
		if x.IsUpload() && isVideoType(x.Type) && x.Dur > 0 {
			return x.Dur
		}
	}
	return 0
}

// SpriteVTT is the ladder's seek-preview track: one cue per sprite tile,
// each pointing at its tile with a #xywh fragment.
func (g *Grant) SpriteVTT(ctx context.Context, dir string) ([]byte, error) {
	_, _, _, sprite := g.ladder(dir)
	if sprite == nil {
		return nil, ErrNotAllowed
	}
	idx, err := g.r.index(ctx, g.Item, sprite.Track.Index)
	if err != nil {
		return nil, err
	}
	s := idx.Sprite
	if s == nil || s.Cols <= 0 || s.Rows <= 0 || s.Interval <= 0 {
		return nil, ErrNotAllowed
	}
	u, err := g.URL(*sprite, false)
	if err != nil {
		return nil, err
	}
	src, _ := g.Manifest.Get(sprite.From)
	n := s.Cols * s.Rows
	if src.Dur > 0 {
		n = min(n, int(math.Ceil(src.Dur/s.Interval)))
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for t := range n {
		start, end := float64(t)*s.Interval, float64(t+1)*s.Interval
		if src.Dur > 0 {
			end = min(end, src.Dur)
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s#xywh=%d,%d,%d,%d\n", vttTime(start), vttTime(end), u, t%s.Cols*s.W, t/s.Cols*s.H, s.W, s.H)
	}
	return []byte(b.String()), nil
}

// index reads a track index blob through the cache (blobs never change).
func (r *Reader) index(ctx context.Context, item Item, blob string) (*TrackIndex, error) {
	if idx := r.indexes.get(blob); idx != nil {
		return idx, nil
	}
	key, err := item.Blob(blob)
	if err != nil {
		return nil, err
	}
	rc, _, err := r.o.Manifests.store.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	if err != nil {
		return nil, err
	}
	var idx TrackIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("media: track index %s: %w", key, err)
	}
	r.indexes.put(blob, &idx, int64(len(body)))
	return &idx, nil
}

func pickTracks(tracks []File, want []string) []File {
	if want == nil {
		return tracks
	}
	var out []File
	for _, t := range tracks {
		if slices.Contains(want, t.Track.ID) || t.Track.Lang != "" && slices.Contains(want, t.Track.Lang) {
			out = append(out, t)
		}
	}
	return out
}

// uniqueName is a NAME attribute unique within its group.
func uniqueName(seen map[string]bool, t *Track) string {
	n := quotable(cmpOr(cmpOr(t.Label, t.Lang), t.ID))
	if seen[n] {
		n += " (" + quotable(t.ID) + ")"
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

// quotable strips what an HLS quoted-string may not hold.
func quotable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

func seconds(s float64) string { return fmt.Sprintf("%.3f", s) }

func vttTime(s float64) string {
	ms := int64(math.Round(s * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// indexCache keeps decoded track indexes by blob, bounded by their size.
type indexCache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List
	items map[string]*list.Element
}

type indexEntry struct {
	blob string
	idx  *TrackIndex
	cost int64
}

func newIndexCache(max int64) *indexCache {
	return &indexCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *indexCache) get(blob string) *TrackIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[blob]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*indexEntry).idx
	}
	return nil
}

func (c *indexCache) put(blob string, idx *TrackIndex, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[blob]; ok || size*4 > c.max {
		return
	}
	c.items[blob] = c.order.PushFront(&indexEntry{blob, idx, size * 4})
	c.used += size * 4
	for c.used > c.max {
		e := c.order.Back()
		v := e.Value.(*indexEntry)
		c.order.Remove(e)
		delete(c.items, v.blob)
		c.used -= v.cost
	}
}
