package video

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// HLS outputs under the preset's directory To:
//
//	{To}{rung}-{codec}.mp4  a video rendition (Track video)
//	{To}audio-{id}.mp4      an audio track of the source (Track audio)
//	{To}subs-{id}.vtt       a text track of the source (Track subs)
//	{To}sprite.jpg          the seek sprite (Track sprite)
//
// Byte-range tracks and the sprite keep their segments or grid in a
// TrackIndex blob (Track.Index).
func renditionPath(to string, n int, c media.Codec) string {
	return fmt.Sprintf("%s%d-%s.mp4", to, n, c)
}
func audioPath(to, id string) string { return to + "audio-" + id + ".mp4" }
func subsPath(to, id string) string  { return to + "subs-" + id + ".vtt" }
func spritePath(to string) string    { return to + "sprite.jpg" }

// isEncode reports a preset of the encode runs (HLS or MP4).
func isEncode(p *media.Private) bool { return p.HLS != nil || p.MP4 != nil }

// fp is the fingerprint of preset p's outputs from upload f (an HLS
// preset's source text tracks have subsFP).
func (e *Encoder) fp(p *media.Private, f media.File) string {
	switch {
	case p.HLS != nil:
		return e.hlsFP(f, p.HLS)
	case p.MP4 != nil:
		return mp4FP(f, p.MP4)
	case p.Audio != nil:
		return audioFP(f, p.Audio)
	case p.Subtitles != nil:
		return subtitleFP(f, p.Subtitles)
	}
	return ""
}

// subsFP is the fingerprint of the source text tracks an HLS preset
// extracts: a cleaning change re-extracts them without re-encoding.
func subsFP(f media.File, h *media.HLS) string { return media.SpecFP(f, h, cleanRecipe) }

// codecs are the codecs a preset's runs encode.
func (e *Encoder) codecs(p *media.Private) []media.Codec {
	if p.MP4 != nil {
		return []media.Codec{media.CodecH264}
	}
	return e.c.Codecs
}

func profile(p *media.Private) string {
	if p.MP4 != nil {
		return p.MP4.Profile
	}
	return p.HLS.Profile
}

// stages are an encode preset's runs for a probed source, smallest rung
// first: the HLS ladder, or the MP4's rung (none above the source).
func stages(p *media.Private, pl plan) ([]rung, error) {
	if p.MP4 != nil {
		if r, ok := pl.rung(p.MP4); ok {
			return []rung{r}, nil
		}
		return nil, nil
	}
	rs, err := pl.ladder(p.HLS)
	slices.Reverse(rs)
	return rs, err
}

// runSpec names a run's preset and fingerprint: "{preset}@{fp}".
func runSpec(p *media.Private, fp string) string { return p.Name + "@" + fp }

// runPreset is the encode preset of upload f whose current fingerprint is
// the run's spec; false when the upload, the preset or its spec changed.
func (e *Encoder) runPreset(k *media.Kind, f media.File, spec string) (*media.Private, string, bool) {
	name, fp, _ := strings.Cut(spec, "@")
	for _, p := range k.PrivateFor(f.Path) {
		if p.Name == name && isEncode(p) && e.fp(p, f) == fp {
			return p, fp, true
		}
	}
	return nil, "", false
}

// stagesDone is how many of stages (smallest rung first) outs hold with fp
// in every codec.
func stagesDone(outs []media.File, to, fp string, stages []rung, codecs []media.Codec) int {
	for k, s := range stages {
		for _, c := range codecs {
			if !slices.ContainsFunc(outs, func(o media.File) bool { return o.Path == renditionPath(to, s.n, c) && o.FP == fp }) {
				return k
			}
		}
	}
	return len(stages)
}

// todo reports whether preset p of upload f is to be produced: pending, or
// an output missing or of another fingerprint. An MP4 above the measured
// source has no output.
func (e *Encoder) todo(m *media.Manifest, f media.File, p *media.Private) bool {
	if slices.Contains(f.Pending, p.Name) {
		return true
	}
	outs := m.Outputs(f.Path, p.Name)
	fp := e.fp(p, f)
	switch {
	case p.MP4 != nil && len(outs) == 0:
		return f.W == 0 || p.MP4.Rung <= min(f.W, f.H)
	case len(outs) == 0:
		return true
	case p.HLS != nil:
		sub := subsFP(f, p.HLS)
		return slices.ContainsFunc(outs, func(o media.File) bool {
			if trackKind(o) == media.TrackSubs {
				return o.FP != sub
			}
			return o.Track == nil || o.FP != fp
		})
	}
	return slices.ContainsFunc(outs, func(o media.File) bool { return o.FP != fp })
}

func trackKind(o media.File) string {
	if o.Track == nil {
		return ""
	}
	return o.Track.Kind
}

// orderHLS sorts an HLS preset's outputs: renditions by codec (the ladder's
// order), largest first, then audio, text tracks and the sprite.
func orderHLS(outs []media.File, codecs []media.Codec) {
	kinds := []string{media.TrackVideo, media.TrackAudio, media.TrackSubs, media.TrackSprite}
	slices.SortStableFunc(outs, func(a, b media.File) int {
		if d := slices.Index(kinds, trackKind(a)) - slices.Index(kinds, trackKind(b)); d != 0 {
			return d
		}
		if trackKind(a) != media.TrackVideo {
			return 0
		}
		return cmp.Or(slices.Index(codecs, media.Codec(a.Track.Codec))-slices.Index(codecs, media.Codec(b.Track.Codec)), b.W*b.H-a.W*a.H)
	})
}

// stream uploads a single-file fMP4 track (dir/name.mp4, after validating
// its playlist's byte ranges) and its index, as an output at path.
func (e *Encoder) stream(ctx context.Context, item media.Item, dir, name, path, contentType string, fp *fileProgress) (media.File, error) {
	file := filepath.Join(dir, name+".mp4")
	st, err := os.Stat(file)
	if err != nil {
		return media.File{}, err
	}
	pl, err := parsePlaylist(filepath.Join(dir, name+".m3u8"), st.Size())
	if err != nil {
		return media.File{}, err
	}
	blob, size, err := e.put(ctx, item, file, contentType, fp)
	if err != nil {
		return media.File{}, err
	}
	index, err := e.putJSON(ctx, item, media.TrackIndex{Segments: pl.segments})
	if err != nil {
		return media.File{}, err
	}
	peak, avg := bandwidth(pl.segments)
	var dur float64
	for _, s := range pl.segments {
		dur += s.Seconds
	}
	return media.File{Path: path, Blob: blob, Type: contentType, Size: size, Dur: math.Round(dur*1000) / 1000,
		Track: &media.Track{Bandwidth: peak, Average: avg, Index: index}}, nil
}

// blobs lists the blobs outputs reference (for checkOutputs and DropIfDeleted).
func blobs(outs ...media.File) []string {
	var out []string
	for _, o := range outs {
		out = append(out, o.Blob)
		if o.Track != nil && o.Track.Index != "" {
			out = append(out, o.Track.Index)
		}
	}
	return out
}

// publish records outputs with edit under the manifest lock, after checking
// every blob exists (the lock fences the sweep). A deleted folder drops the
// blobs written for it; errStale discards them (the sweep collects them).
func (e *Encoder) publish(ctx context.Context, item media.Item, outs []media.File, edit func(*media.Manifest) error) error {
	if testBeforePublish != nil {
		testBeforePublish()
	}
	_, err := e.ms.EditExisting(ctx, item.Ref(), func(m *media.Manifest) error {
		if err := edit(m); err != nil {
			return err
		}
		return e.checkOutputs(ctx, item, blobs(outs...)...)
	})
	if errors.Is(err, media.ErrNotFound) {
		return e.ms.DropIfDeleted(ctx, item.Ref(), blobs(outs...))
	}
	return err
}

// current is upload f while it holds blob: the edit's fence.
func current(m *media.Manifest, path, blob string) (media.File, error) {
	f, ok := m.Get(path)
	if !ok || f.Blob != blob || f.Gone {
		return f, errStale
	}
	return f, nil
}
