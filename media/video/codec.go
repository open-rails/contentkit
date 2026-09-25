package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// DefaultCodecs is the ladder's codecs when Config.Codecs is empty: HEVC,
// which players that decode it prefer, and H.264, which plays everywhere.
var DefaultCodecs = []media.Codec{media.CodecHEVC, media.CodecH264}

// Config.Encoder values.
const (
	EncoderAuto  = "auto"  // per codec: NVENC when a probe encode works, else the CPU encoder
	EncoderCPU   = "cpu"   // libx264, libx265, SVT-AV1
	EncoderNVENC = "nvenc" // NVENC for every codec; decoding and scaling stay on the CPU
)

// cpuEncoders and nvencEncoders are each codec's ffmpeg encoders.
var (
	cpuEncoders   = map[media.Codec]string{media.CodecH264: "libx264", media.CodecHEVC: "libx265", media.CodecAV1: "libsvtav1"}
	nvencEncoders = map[media.Codec]string{media.CodecH264: "h264_nvenc", media.CodecHEVC: "hevc_nvenc", media.CodecAV1: "av1_nvenc"}
)

// svtAV1Preset is SVT-AV1's speed: at 1080p it takes 1.34× x264 fast's time
// for 64% fewer bits at the same VMAF (MEDIA-ENCODING-CAPACITY.md).
const svtAV1Preset = 8

// nvencCQOffset maps a rung's CRF to an NVENC CQ of about the same top-rung
// VMAF; lower rungs cost NVENC 30-60% more bits (bench_test.go). AV1's CQ
// scale sits 6 above SVT-AV1's CRF (CQ 38 ≈ CRF 32 at 1080p).
var nvencCQOffset = map[media.Codec]int{media.CodecH264: 4, media.CodecHEVC: 4, media.CodecAV1: 6}

// encoding is how a file's renditions are encoded.
type encoding struct {
	encoders  map[media.Codec]string // ffmpeg encoder per codec
	threads   int
	preset    string // x264/x265 preset of rungs up to 1080
	topPreset string // x264/x265 preset of the rungs above
	animation bool
}

// streamArgs are output stream i's settings: rung r in codec c at the rung's
// capped CRF (NVENC: CQ) with a 2 s VBV buffer, closed GOPs and no scene-cut
// keyframes, so every rendition has its keyframes where -force_key_frames
// puts them. HEVC is tagged hvc1 (parameter sets in the sample entry), which
// Safari requires.
func (e encoding) streamArgs(i int, r rung, c media.Codec) []string {
	o := func(name string) string { return fmt.Sprintf("-%s:v:%d", name, i) }
	rt := r.rate(c)
	enc := e.encoders[c]
	args := []string{o("c"), enc, o("maxrate"), fmt.Sprintf("%dk", rt.maxrate), o("bufsize"), fmt.Sprintf("%dk", 2*rt.maxrate)}
	preset := e.preset
	if r.n > 1080 {
		preset = e.topPreset
	}
	threads := strconv.Itoa(e.threads)
	nvenc := func(cq int) []string {
		return []string{o("preset"), "p5", o("tune"), "hq", o("rc"), "vbr", o("cq"), strconv.Itoa(cq), o("b"), "0",
			o("spatial-aq"), "1", o("temporal-aq"), "1", o("rc-lookahead"), "20", o("forced-idr"), "1", o("no-scenecut"), "1"}
	}
	switch enc {
	case "libx264", "h264_nvenc":
		args = append(args, o("profile"), "high")
		if r.level != "" {
			args = append(args, o("level"), r.level)
		}
		if enc == "h264_nvenc" {
			return append(append(args, nvenc(rt.crf+nvencCQOffset[c])...), o("bf"), "3", o("b_ref_mode"), "middle")
		}
		args = append(args, o("preset"), preset, o("crf"), strconv.Itoa(rt.crf), o("sc_threshold"), "0", o("threads"), threads)
		if e.animation {
			args = append(args, o("tune"), "animation")
		}
	case "libx265":
		// x265 sizes its pool by the host's cores, not -threads.
		// Main tier: players decode High tier less widely; x265 raises the level instead.
		params := "pools=" + threads + ":open-gop=0:scenecut=0:high-tier=0:log-level=error"
		args = append(args, o("profile"), "main", o("tag"), "hvc1", o("preset"), preset, o("crf"), strconv.Itoa(rt.crf), o("forced-idr"), "1",
			o("x265-params"), params)
		if e.animation {
			args = append(args, o("tune"), "animation")
		}
	case "hevc_nvenc":
		args = append(append(args, o("profile"), "main", o("tag"), "hvc1"), nvenc(rt.crf+nvencCQOffset[c])...)
	case "libsvtav1":
		args = append(args, o("preset"), strconv.Itoa(svtAV1Preset), o("crf"), strconv.Itoa(rt.crf), o("threads"), threads,
			o("svtav1-params"), fmt.Sprintf("tune=0:mbr=%d", rt.maxrate))
	case "av1_nvenc":
		args = append(args, nvenc(rt.crf+nvencCQOffset[c])...)
	}
	return args
}

// resolveEncoders picks each codec's encoder for mode and checks it with a
// probe encode.
func resolveEncoders(ctx context.Context, dir, mode string, codecs []media.Codec) (map[media.Codec]string, error) {
	out := map[media.Codec]string{}
	for _, c := range codecs {
		if cpuEncoders[c] == "" {
			return nil, fmt.Errorf("media/video: unknown codec %q", c)
		}
		switch mode {
		case EncoderAuto:
			out[c] = nvencEncoders[c]
			if probeEncoder(ctx, dir, c, out[c]) == nil {
				continue
			}
			fallthrough
		case EncoderCPU:
			out[c] = cpuEncoders[c]
		case EncoderNVENC:
			out[c] = nvencEncoders[c]
		default:
			return nil, fmt.Errorf("media/video: unknown Encoder %q", mode)
		}
		if err := probeEncoder(ctx, dir, c, out[c]); err != nil {
			return nil, fmt.Errorf("media/video: %s unusable: %w", out[c], err)
		}
	}
	return out, nil
}

// probeEncoder encodes a second of 256×256 video with the rendition settings
// and a keyframe forced at 0.5 s, and requires that keyframe: an encoder
// that ignores forced keyframes (libsvtav1 before ffmpeg 7 and SVT-AV1 2)
// would misalign segments.
func probeEncoder(ctx context.Context, dir string, c media.Codec, enc string) error {
	d, err := os.MkdirTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(d)
	out := filepath.Join(d, "probe.mp4")
	e := encoding{encoders: map[media.Codec]string{c: enc}, threads: 2, preset: "ultrafast", topPreset: "ultrafast"}
	args := append([]string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=s=256x256:r=10:d=1"},
		e.streamArgs(0, rung{n: 256, w: 256, h: 256}, c)...)
	args = append(args, "-pix_fmt", "yuv420p", "-force_key_frames", "expr:gte(t,n_forced*0.5)", "-y", out)
	if _, err := command(ctx, "ffmpeg", args...); err != nil {
		return err
	}
	pk, err := command(ctx, "ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", out)
	if err != nil {
		return err
	}
	if strings.Count(string(pk), ",K") < 2 { // every encoder's own GOP is longer than 1 s
		return errors.New("forced keyframes are ignored")
	}
	return nil
}

// codecString is the RFC 6381 codecs value of an fMP4 rendition, from the
// decoder configuration record in its init segment.
func codecString(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 64<<10)
	n, err := f.ReadAt(b, 0)
	if n == 0 {
		return "", err
	}
	b = b[:n]
	rec := func(box string, size int) []byte {
		i := bytes.Index(b, []byte(box))
		if i < 0 || len(b) < i+4+size {
			return nil
		}
		return b[i+4 : i+4+size]
	}
	if r := rec("avcC", 4); r != nil {
		return fmt.Sprintf("avc1.%02x%02x%02x", r[1], r[2], r[3]), nil
	}
	if r := rec("hvcC", 13); r != nil {
		return hvcCodec(r), nil
	}
	if r := rec("av1C", 3); r != nil {
		depth := 8
		if r[2]&0x40 != 0 {
			depth = map[bool]int{false: 10, true: 12}[r[2]&0x20 != 0 && r[1]>>5 == 2]
		}
		return fmt.Sprintf("av01.%d.%02d%s.%02d", r[1]>>5, r[1]&0x1f, map[bool]string{false: "M", true: "H"}[r[2]&0x80 != 0], depth), nil
	}
	return "", fmt.Errorf("%s: no avcC, hvcC or av1C", path)
}

// hvcCodec formats an HEVCDecoderConfigurationRecord's profile, tier, level
// and constraints (ISO/IEC 14496-15 Annex E), e.g. "hvc1.1.6.L120.90".
func hvcCodec(r []byte) string {
	space := []string{"", "A", "B", "C"}[r[1]>>6]
	tier := map[bool]string{false: "L", true: "H"}[r[1]&0x20 != 0]
	compat := bits.Reverse32(binary.BigEndian.Uint32(r[2:6]))
	cons := r[6:12]
	for len(cons) > 0 && cons[len(cons)-1] == 0 {
		cons = cons[:len(cons)-1]
	}
	s := fmt.Sprintf("hvc1.%s%d.%x.%s%d", space, r[1]&0x1f, compat, tier, r[12])
	for _, c := range cons {
		s += fmt.Sprintf(".%x", c)
	}
	return s
}

// orderVideo sorts renditions by codec (the ladder's order), then largest
// rung first.
func orderVideo(video []media.Rendition, codecs []media.Codec) {
	slices.SortStableFunc(video, func(a, b media.Rendition) int {
		if d := slices.Index(codecs, a.Codec) - slices.Index(codecs, b.Codec); d != 0 {
			return d
		}
		return b.Rung - a.Rung
	})
}
