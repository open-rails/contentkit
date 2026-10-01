package video

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// audioRecipe is the Audio producer's encode: its outputs carry its hash
// with the source and the preset's spec (media.SpecFP).
const audioRecipe = "aac-lc-128k-48k-2ch|hls-fmp4-4s|m4a-faststart|gain:r128-after-downmix/tp-1.5|v3"

func audioFP(f media.File, a *media.Audio) string { return media.SpecFP(f, a, audioRecipe) }

// audioDemuxers are the containers an audio source may be.
var audioDemuxers = []string{"mp3", "wav", "w64", "flac", "ogg", "mov", "aac", "matroska", "aiff", "caf", "asf"}

func isAudio(p *media.Private) bool { return p.Audio != nil }

// audio produces an item's stale Audio presets (only args.Preset when set;
// every one with args.Force).
func (e *Encoder) audio(ctx context.Context, item media.Item, args workqueue.AudioArgs, report Report) error {
	if e.full(ctx, item) {
		return nil
	}
	if args.Force {
		if err := e.force(ctx, item, args.Preset, isAudio); err != nil {
			return err
		}
	}
	man, _, err := e.ms.Get(ctx, item.Ref())
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	type todo struct {
		f media.File
		p *media.Private
	}
	var work []todo
	var names []string
	for _, f := range man.Files {
		if !f.IsUpload() || f.Gone || f.Blob == "" || f.Fail() != nil {
			continue
		}
		for _, p := range item.Kind().PrivateFor(f.Path) {
			if p.Audio != nil && (args.Preset == "" || p.Name == args.Preset) && e.todo(man, f, p) {
				work = append(work, todo{f, p})
				if !slices.Contains(names, f.Path) {
					names = append(names, f.Path)
				}
			}
		}
	}
	prog := newProgress(ctx, report, e.c.ProgressInterval, time.Now, names)
	var errs []error
	for _, w := range work {
		err := e.audioFile(ctx, item, w.f, w.p, prog.file(w.f.Path))
		prog.done(w.f.Path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errs = append(errs, e.settle(ctx, item, w.f, err))
	}
	return errors.Join(errs...)
}

// audioFile encodes upload f's default (else first) audio stream,
// optionally loudness-normalized, to AAC in a single-file fMP4 HLS track
// ({To}audio.mp4) and a faststart M4A remuxed from it ({To}audio.m4a).
func (e *Encoder) audioFile(ctx context.Context, item media.Item, f media.File, p *media.Private, fp *fileProgress) error {
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "source")
	if err := e.fetch(ctx, item, f.Blob, src, fp); errors.Is(err, media.ErrNotFound) {
		return errStale
	} else if err != nil {
		return err
	}
	fp.set(media.PhaseProbing)
	pr, err := probeWith(ctx, src, audioDemuxers)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PermanentError{fmt.Errorf("unreadable audio: %w", err)}
	}
	t, d, err := audioPlan(pr)
	if err != nil {
		return &PermanentError{err}
	}
	// The running time is the packets', not the container's word.
	if scan, err := scanPackets(ctx, src, t.index, inputOptions(audioDemuxers)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PermanentError{fmt.Errorf("unreadable audio packets: %w", err)}
	} else if scan.End > scan.Start {
		d = scan.End - scan.Start
	}
	u, _ := item.Kind().UploadOf(f.Path)
	if limit := u.Video.Limits().MaxSeconds; d > limit {
		return &PermanentError{&media.ImageError{Code: media.CodeVideoTooLong, Message: fmt.Sprintf("the audio runs %.0f seconds; at most %.0f", d, limit),
			Details: media.ErrorDetails{Seconds: math.Round(d), MaxSeconds: limit}}}
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	a := p.Audio
	e.c.Logger.InfoContext(ctx, "media/video: encoding audio", "ref", item.Ref().String(), "path", f.Path, "duration", d, "loudness", a.Loudness)
	fp.stage(1, 1)
	fp.probed(d, out)
	// Measured after the downmix, so a 5.1 source is normalized as heard in stereo.
	filter := audioFormat
	encoded := fp.encoded
	if a.Loudness != 0 {
		// Two passes: progress is half the measure, then half the encode.
		gain, err := measureGain(ctx, src, t.index, a.Loudness, func(t float64) { fp.encoded(t / 2) })
		if err != nil {
			return err
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
	if _, err := ffmpegProgress(ctx, encoded, args...); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	fp.set(media.PhaseMuxing)
	m4a := filepath.Join(out, "audio.m4a")
	own := inputOptions([]string{"mov"})
	if _, err := command(ctx, "ffmpeg", append(append([]string{"-v", "error", "-nostdin"}, own...), "-i", filepath.Join(out, "a1.mp4"),
		"-map", "0:a:0", "-c", "copy", "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact",
		"-movflags", "+faststart", "-f", "mp4", "-y", m4a)...); err != nil {
		return err
	}
	fp.uploads(fileSize(filepath.Join(out, "a1.mp4")) + fileSize(m4a))
	fp.set(media.PhaseUploading)
	to, fprint, dur := item.Kind().OutputPath(p, f.Path), audioFP(f, a), math.Round(d*1000)/1000
	hls, err := e.stream(ctx, item, out, "a1", to+"audio.mp4", "audio/mp4", fp)
	if err != nil {
		return err
	}
	hls.FP, hls.Track = fprint, &media.Track{Kind: media.TrackAudio, ID: "a1", Lang: t.lang, Label: t.label, Default: true,
		Bandwidth: hls.Track.Bandwidth, Codecs: "mp4a.40.2", Index: hls.Track.Index}
	blob, size, err := e.put(ctx, item, m4a, "audio/mp4", fp)
	if err != nil {
		return err
	}
	outs := []media.File{hls, {Path: to + "audio.m4a", Blob: blob, Type: "audio/mp4", Size: size, Dur: dur, FP: fprint}}
	fp.set(media.PhasePublishing)
	return e.publish(ctx, item, outs, func(m *media.Manifest) error {
		g, err := current(m, f.Path, f.Blob)
		if err != nil || audioFP(g, a) != fprint {
			return errStale
		}
		m.Files[m.Find(g.Path)].Dur = dur
		return m.SetOutputs(g.Path, p.Name, outs)
	})
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
	b, _, err := ffmpegProgressTail(ctx, progress, args...)
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
