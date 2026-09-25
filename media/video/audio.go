package video

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// audioRecipe is an audio file's encode; its hls, variant and download
// carry its hash (AudioSpec) and are re-encoded when it changes.
const audioRecipe = "aac-lc-128k-48k-2ch|hls-fmp4-4s|m4a-faststart|gain:%g/r128-after-downmix/tp-1.5|v2"

// AudioSpec identifies the audio recipe under a's settings.
func AudioSpec(a media.Audio) string {
	s := sha256.Sum256(fmt.Appendf(nil, audioRecipe, a.Loudness))
	return hex.EncodeToString(s[:4])
}

// audioDemuxers are the containers an audio source may be.
var audioDemuxers = []string{"mp3", "wav", "w64", "flac", "ogg", "mov", "aac", "matroska", "aiff", "caf", "asf"}

// IsAudio reports whether a manifest file is an audio file (encoded when
// the kind has media.Audio).
func IsAudio(f media.File) bool { return strings.HasPrefix(f.Type, "audio/") }

// audioFresh reports an audio file whose outputs (or failure) match its
// source and spec.
func audioFresh(m *media.Manifest, f media.File, spec string) bool {
	h := f.HLS
	if h == nil || h.Source != f.Source() || h.Spec != spec {
		return false
	}
	if h.Error != "" {
		return true
	}
	v, ok := f.Variants[media.AudioVariant]
	d, dok := m.Downloads[media.AudioDownloadKey(f.Name)]
	return len(h.Audio) == 1 && ok && v.Spec == spec && dok && d.Spec == spec && d.Inputs == h.Source
}

func audioDownload(key string, d media.Download) (file string, ok bool) {
	file, ok = strings.CutSuffix(key, "-audio")
	return file, ok && file != "" && d.Type == "audio/mp4"
}

// audioFile encodes one audio file: its default (else first) audio stream, optionally
// loudness-normalized, to AAC in a single-file fMP4 HLS track and a
// faststart M4A remuxed from it, promoted in one manifest edit fenced on the
// original's ETag. It returns the source, placed if it was staged.
func (e *Encoder) audioFile(ctx context.Context, ms *media.Manifests, item media.Item, a media.Audio, name, source string, fp *fileProgress) (string, error) {
	spec := AudioSpec(a)
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return source, err
	}
	defer os.RemoveAll(dir)
	srcKey, placed, srcObj, err := e.source(ctx, ms, item, name, source, filepath.Join(dir, "source"), fp)
	if err != nil || srcKey == "" {
		return source, err
	}
	source = placed
	src := filepath.Join(dir, "source")

	fp.set(media.PhaseProbing)
	pr, err := probeWith(ctx, src, audioDemuxers)
	if err != nil {
		if ctx.Err() != nil {
			return source, ctx.Err()
		}
		return source, &PermanentError{fmt.Errorf("unreadable audio: %w", err)}
	}
	t, d, err := audioPlan(pr)
	if err != nil {
		return source, &PermanentError{err}
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return source, err
	}
	e.c.Logger.InfoContext(ctx, "media/video: encoding audio", "key", srcKey, "duration", d, "loudness", a.Loudness)
	fp.stage(1, 1)
	fp.probed(d, out)
	// Measured after the downmix, so a 5.1 source is normalized as heard in stereo.
	filter := audioFormat
	encoded := fp.encoded
	if a.Loudness != 0 {
		// Two passes: progress is half the measure, then half the encode.
		gain, err := measureGain(ctx, src, t.index, a.Loudness, func(t float64) { fp.encoded(t / 2) })
		if err != nil {
			return source, err
		}
		if gain != 0 {
			filter += fmt.Sprintf(",volume=%.2fdB", gain)
		}
		encoded = func(t float64) { fp.encoded(d/2 + t/2) }
	}
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions(audioDemuxers)...)
	args = append(args, "-i", src, "-map", fmt.Sprintf("0:%d", t.index), "-map_metadata", "-1", "-af", filter)
	args = append(args, "-c:a", "aac", "-b:a", "128k")
	args = append(args, hlsArgs(filepath.Join(out, "a1.mp4"), filepath.Join(out, "a1.m3u8"))...)
	if err := ffmpegProgress(ctx, encoded, args...); err != nil {
		return source, err
	}
	if err := os.Remove(src); err != nil {
		return source, err
	}
	fp.set(media.PhaseMuxing)
	m4a := filepath.Join(out, "audio.m4a")
	own := inputOptions([]string{"mov"})
	if _, err := command(ctx, "ffmpeg", append(append([]string{"-v", "error", "-nostdin"}, own...), "-i", filepath.Join(out, "a1.mp4"),
		"-map", "0:a:0", "-c", "copy", "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-movflags", "+faststart", "-f", "mp4", "-y", m4a)...); err != nil {
		return source, err
	}
	fp.uploads(fileSize(filepath.Join(out, "a1.mp4")) + fileSize(m4a))
	fp.set(media.PhaseUploading)
	blob, pl, err := e.stream(ctx, item, out, "a1", "audio/mp4", fp)
	if err != nil {
		return source, err
	}
	m4aBlob, m4aSize, err := e.put(ctx, item, m4a, "audio/mp4", fp)
	if err != nil {
		return source, err
	}
	peak, _ := bandwidth(pl.segments)
	hls := &media.HLS{Source: source, Spec: spec, Audio: []media.AudioTrack{{ID: "a1", Lang: t.lang, Label: t.label, Default: true,
		Bandwidth: peak, Codecs: "mp4a.40.2", Blob: blob, Segments: pl.segments}}}
	variant := media.Variant{Blob: m4aBlob, Spec: spec, Type: "audio/mp4", Size: m4aSize}
	download := media.Download{Blob: m4aBlob, Type: "audio/mp4", Size: m4aSize, Spec: spec, Inputs: source}

	fp.set(media.PhasePublishing)
	if testBeforePromote != nil {
		testBeforePromote()
	}
	if obj, err := e.c.Store.Head(ctx, srcKey); errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != srcObj.ETag {
		return source, e.stale(ctx, ms, item, name, source, errStale)
	} else if err != nil {
		return source, err
	}
	_, err = ms.Edit(ctx, item.Ref(), func(m *media.Manifest) error {
		i := m.File(name)
		if i < 0 || m.Files[i].Source() != source {
			return errStale
		}
		f := &m.Files[i]
		f.HLS = hls
		if f.Variants == nil {
			f.Variants = map[string]media.Variant{}
		}
		f.Variants[media.AudioVariant] = variant
		if f.Meta == nil {
			f.Meta = map[string]any{}
		}
		f.Meta["duration"] = d
		if m.Downloads == nil {
			m.Downloads = map[string]media.Download{}
		}
		m.Downloads[media.AudioDownloadKey(name)] = download
		return nil
	})
	if errors.Is(err, errStale) {
		return source, e.stale(ctx, ms, item, name, source, err)
	}
	return source, err
}

// audioPlan picks the source's first audio stream (the default one when
// flagged) and its duration.
func audioPlan(p probeResult) (track, float64, error) {
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil || d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return track{}, 0, errors.New("audio has no valid duration")
	}
	var pick *probeStream
	for i, s := range p.Streams {
		if s.CodecType == "audio" && (pick == nil || s.Disposition.Default == 1 && pick.Disposition.Default == 0) {
			pick = &p.Streams[i]
		}
	}
	if pick == nil {
		return track{}, 0, errors.New("source has no audio stream")
	}
	// Audio files tag the container (ID3, Vorbis comments), not the stream.
	s := *pick
	s.Tags.Language = cmp.Or(s.Tags.Language, p.Format.Tags.Language)
	s.Tags.Title = cmp.Or(s.Tags.Title, p.Format.Tags.Title)
	pick = &s
	t := track{index: pick.Index, def: true}
	t.lang, t.iso6392 = normalizeLanguage(pick.Tags.Language)
	t.label = trackLabel(*pick, t.lang, 1, map[string]int{})
	return t, d, nil
}

// audioFormat is every audio output's resample and downmix (48 kHz stereo,
// the AAC encoder's float planar). The sample format is pinned so the
// measuring pass sees the encode's samples: swresample scales a downmix
// differently when it has to produce double for loudnorm (5.1 read 7.6 LU low).
const audioFormat = "aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo"

// measureGain measures the stream's integrated loudness and true peak (EBU
// R128) after audioFormat and returns the linear gain in dB that reaches
// target LUFS without the true peak passing -1.5 dBTP: min(target - I,
// -1.5 - TP). A plain gain is linear and deterministic, where a second
// loudnorm pass turns dynamic above LRA 11 or a peak it would clip. Silence
// gets 0.
func measureGain(ctx context.Context, src string, stream int, target float64, progress func(float64)) (float64, error) {
	args := append([]string{"-hide_banner", "-nostdin"}, inputOptions(audioDemuxers)...)
	args = append(args, "-i", src, "-map", fmt.Sprintf("0:%d", stream),
		"-af", fmt.Sprintf("%s,loudnorm=I=%g:TP=-1.5:LRA=11:print_format=json", audioFormat, target), "-f", "null", "-")
	b, err := ffmpegProgressTail(ctx, progress, args...)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &PermanentError{fmt.Errorf("loudness analysis: %w", err)}
	}
	i, j := bytes.LastIndexByte(b, '{'), bytes.LastIndexByte(b, '}')
	var m struct {
		I  string `json:"input_i"`
		TP string `json:"input_tp"`
	}
	if i < 0 || j < i || json.Unmarshal(b[i:j+1], &m) != nil {
		return 0, errors.New("media/video: loudness analysis printed no measurement")
	}
	lufs, err1 := strconv.ParseFloat(m.I, 64)
	peak, err2 := strconv.ParseFloat(m.TP, 64)
	if err1 != nil || err2 != nil || math.IsInf(lufs, 0) || math.IsNaN(lufs) || math.IsNaN(peak) {
		return 0, nil
	}
	if math.IsInf(peak, 0) {
		peak = -1.5
	}
	return math.Round(min(target-lufs, -1.5-peak)*100) / 100, nil
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
