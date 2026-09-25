package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// Passthrough: a source that already is a compliant rendition of the top
// rung in one of the ladder's codecs is stream-copied instead of encoded in
// that codec. Every check is strict; any doubt encodes. The copy keeps the
// source's frames and timestamps, so the source must be constant-rate H.264
// High/Main or HEVC Main tagged hvc1 (8-bit 4:2:0, progressive, level ≤ 5.2,
// square pixels, unrotated) at exactly the rung's frame, within the rung's
// bitrate cap in its codec, in MP4/MOV, and IDR-coded (closed GOP) on the
// frame that starts each segment, where the encoded rungs put theirs.

type sourcePacket struct {
	t         float64 // seconds from the container start
	key       bool
	pos, size int64
}

// passthroughable reports whether top can be copied from src as a
// rendition in codec c, and why not.
func passthroughable(ctx context.Context, src string, p plan, top rung) (c media.Codec, ok bool, why string) {
	c, ok, why = passthroughCandidate(p, top)
	if !ok {
		return c, false, why
	}
	f, err := os.Open(src)
	if err != nil {
		return c, false, err.Error()
	}
	defer f.Close()
	pkts, err := sourcePackets(ctx, src, p)
	if err != nil || len(pkts) < 2 {
		return c, false, fmt.Sprintf("packets: %v", err)
	}
	maxrate := top.rate(c).maxrate
	// Constant rate: every frame one interval after the one before.
	frame := pkts[1].t - pkts[0].t
	var bytes int64
	for i, pk := range pkts {
		if i > 0 && math.Abs(pk.t-pkts[i-1].t-frame) > frame/100 {
			return c, false, fmt.Sprintf("variable frame rate at %.3f s", pk.t)
		}
		bytes += pk.size
	}
	if frame <= 0 || math.Abs(1/frame-p.fps) > 0.01*p.fps {
		return c, false, "frame rate"
	}
	if avg := float64(bytes*8) / p.duration / 1000; avg > float64(maxrate) {
		return c, false, fmt.Sprintf("%.0f kbit/s over the rung's %d", avg, maxrate)
	}
	next := 0.0
	var window int64
	windowStart := 0.0
	for _, pk := range pkts {
		if pk.t >= next-1e-9 {
			if !pk.key {
				return c, false, fmt.Sprintf("no keyframe at %.3f s", pk.t)
			}
			if idr, err := isIDR(f, pk, c); err != nil || !idr {
				return c, false, fmt.Sprintf("keyframe at %.3f s is not an IDR (%v)", pk.t, err)
			}
			next += segmentSeconds
			for pk.t >= next-1e-9 {
				next += segmentSeconds
			}
		}
		if pk.t-windowStart >= segmentSeconds {
			if kbit := float64(window*8) / segmentSeconds / 1000; kbit > 2*float64(maxrate) {
				return c, false, fmt.Sprintf("%.0f kbit/s peak near %.0f s", kbit, windowStart)
			}
			window, windowStart = 0, pk.t
		}
		window += pk.size
	}
	return c, true, ""
}

// The planner only checks probe metadata. Packet and IDR validation belongs
// to the encode worker, where a complete local output is available.
func passthroughCandidate(p plan, top rung) (c media.Codec, ok bool, why string) {
	s := p.stream
	c, maxLevel := media.CodecH264, 52
	if s.CodecName == "hevc" {
		c, maxLevel = media.CodecHEVC, 156 // general_level_idc: 30 × level
	}
	switch {
	case !strings.HasPrefix(p.formatName, "mov,"):
		return c, false, "container " + p.formatName
	case s.CodecName == "h264" && s.Profile != "High" && s.Profile != "Main",
		s.CodecName == "hevc" && (s.Profile != "Main" || s.CodecTag != "hvc1"),
		s.CodecName != "h264" && s.CodecName != "hevc":
		return c, false, "codec " + s.CodecName + " " + s.Profile + " " + s.CodecTag
	case s.PixFmt != "yuv420p" || s.FieldOrder != "progressive":
		return c, false, "format " + s.PixFmt + " " + s.FieldOrder
	case s.Level <= 0 || s.Level > maxLevel:
		return c, false, "level " + strconv.Itoa(s.Level)
	case rotated(s) || p.limitFPS:
		return c, false, "rotated or above the frame-rate cap"
	case s.Width != top.w || s.Height != top.h || p.width != top.w || p.height != top.h:
		return c, false, fmt.Sprintf("frame %dx%d, rung %dx%d", s.Width, s.Height, top.w, top.h)
	}
	if st, err := strconv.ParseFloat(s.StartTime, 64); err != nil || math.Abs(st-p.start) > 1e-6 {
		return c, false, "video starts after the container"
	}
	return c, true, ""
}

// sourcePackets lists the video stream's packets in presentation order.
func sourcePackets(ctx context.Context, src string, p plan) ([]sourcePacket, error) {
	out, err := command(ctx, "ffprobe", append(append([]string{"-v", "error"}, inputOptions(sourceDemuxers)...),
		"-select_streams", strconv.Itoa(p.video), "-show_entries", "packet=pts_time,flags,pos,size", "-of", "csv=p=0", src)...)
	if err != nil {
		return nil, err
	}
	recs, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		return nil, err
	}
	pkts := make([]sourcePacket, 0, len(recs))
	for _, r := range recs {
		if len(r) < 4 {
			return nil, fmt.Errorf("packet %v", r)
		}
		var pk sourcePacket
		t, err1 := strconv.ParseFloat(r[0], 64)
		size, err2 := strconv.ParseInt(r[1], 10, 64)
		pos, err3 := strconv.ParseInt(r[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || size <= 0 || size > 16<<20 || pos < 0 {
			return nil, fmt.Errorf("packet %v", r)
		}
		pk.t, pk.size, pk.pos, pk.key = t-p.start, size, pos, strings.HasPrefix(r[3], "K")
		pkts = append(pkts, pk)
	}
	for i := 1; i < len(pkts); i++ { // insertion sort: decode order is nearly presentation order
		for j := i; j > 0 && pkts[j].t < pkts[j-1].t; j-- {
			pkts[j], pkts[j-1] = pkts[j-1], pkts[j]
		}
	}
	return pkts, nil
}

// isIDR reports whether a length-prefixed (MP4) H.264 or HEVC packet's
// first slice is an IDR slice (HEVC: IDR_W_RADL or IDR_N_LP; a CRA opens a
// GOP).
func isIDR(f *os.File, pk sourcePacket, c media.Codec) (bool, error) {
	b := make([]byte, pk.size)
	if _, err := f.ReadAt(b, pk.pos); err != nil {
		return false, err
	}
	for len(b) >= 5 {
		n := int(binary.BigEndian.Uint32(b))
		if n <= 0 || n > len(b)-4 {
			return false, fmt.Errorf("NAL length %d", n)
		}
		if c == media.CodecHEVC {
			if t := b[4] >> 1 & 0x3f; t < 32 {
				return t == 19 || t == 20, nil
			}
		} else {
			switch b[4] & 0x1f {
			case 5:
				return true, nil
			case 1, 2, 3, 4:
				return false, nil
			}
		}
		b = b[4+n:]
	}
	return false, fmt.Errorf("no slice")
}

// copyRung stream-copies the source's video into the single-file fMP4 v.
func copyRung(ctx context.Context, src, dir string, p plan, v string, c media.Codec) error {
	args := append(append([]string{"-v", "error", "-nostdin"}, inputOptions(sourceDemuxers)...), "-i", src,
		"-map", fmt.Sprintf("0:%d", p.video), "-c", "copy", "-map_metadata", "-1")
	if c == media.CodecHEVC {
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args, hlsArgs(filepath.Join(dir, v+".mp4"), filepath.Join(dir, v+".m3u8"))...)
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

// copyRungRemote stream-copies a short source into its single queued chunk.
func copyRungRemote(ctx context.Context, src, dir string, p plan, v string, c media.Codec) error {
	args := append([]string{"-v", "error", "-nostdin"}, remoteInputOptions(sourceDemuxers)...)
	args = append(args, "-i", src, "-map", fmt.Sprintf("0:%d", p.video),
		"-c", "copy", "-map_metadata", "-1")
	if c == media.CodecHEVC {
		args = append(args, "-tag:v", "hvc1")
	}
	args = append(args, hlsArgs(filepath.Join(dir, v+".mp4"), filepath.Join(dir, v+".m3u8"))...)
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

// sameSegments reports whether rendition v in dir has the segment durations
// of want, so players switch between them at the same times.
func sameSegments(dir, v string, want []media.Segment) bool {
	st, err := os.Stat(filepath.Join(dir, v+".mp4"))
	if err != nil {
		return false
	}
	pl, err := parsePlaylist(filepath.Join(dir, v+".m3u8"), st.Size())
	if err != nil || len(pl.segments) != len(want) {
		return false
	}
	for i, s := range pl.segments {
		if math.Abs(s.Seconds-want[i].Seconds) > 1e-3 {
			return false
		}
	}
	return true
}
