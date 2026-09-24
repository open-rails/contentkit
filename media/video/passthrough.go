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
)

// Passthrough: a source that already is a compliant rendition of the top
// rung is stream-copied instead of encoded. Every check is strict; any
// doubt encodes. The copy keeps the source's frames and timestamps, so the
// source must be constant-rate H.264 High/Main (8-bit 4:2:0, progressive,
// level ≤ 5.2, square pixels, unrotated) at exactly the rung's frame, within
// the rung's bitrate cap, in MP4/MOV, and IDR-coded (closed GOP) on the
// frame that starts each 2 s keyframe interval, where the encoded rungs put
// theirs.

type sourcePacket struct {
	t         float64 // seconds from the container start
	key       bool
	pos, size int64
}

// passthroughable reports whether top can be copied from src, and why not.
func passthroughable(ctx context.Context, src string, p plan, top rung) (bool, string) {
	s := p.stream
	switch {
	case !strings.HasPrefix(p.formatName, "mov,"):
		return false, "container " + p.formatName
	case s.CodecName != "h264" || s.Profile != "High" && s.Profile != "Main":
		return false, "codec " + s.CodecName + " " + s.Profile
	case s.PixFmt != "yuv420p" || s.FieldOrder != "progressive":
		return false, "format " + s.PixFmt + " " + s.FieldOrder
	case s.Level <= 0 || s.Level > 52:
		return false, "level " + strconv.Itoa(s.Level)
	case rotated(s) || p.limitFPS:
		return false, "rotated or above the frame-rate cap"
	case s.Width != top.w || s.Height != top.h || p.width != top.w || p.height != top.h:
		return false, fmt.Sprintf("frame %dx%d, rung %dx%d", s.Width, s.Height, top.w, top.h)
	}
	if st, err := strconv.ParseFloat(s.StartTime, 64); err != nil || math.Abs(st-p.start) > 1e-6 {
		return false, "video starts after the container"
	}
	pkts, err := sourcePackets(ctx, src, p)
	if err != nil || len(pkts) < 2 {
		return false, fmt.Sprintf("packets: %v", err)
	}
	// Constant rate: every frame one interval after the one before.
	frame := pkts[1].t - pkts[0].t
	var bytes int64
	for i, pk := range pkts {
		if i > 0 && math.Abs(pk.t-pkts[i-1].t-frame) > frame/100 {
			return false, fmt.Sprintf("variable frame rate at %.3f s", pk.t)
		}
		bytes += pk.size
	}
	if frame <= 0 || math.Abs(1/frame-p.fps) > 0.01*p.fps {
		return false, "frame rate"
	}
	if avg := float64(bytes*8) / p.duration / 1000; avg > float64(top.maxrate) {
		return false, fmt.Sprintf("%.0f kbit/s over the rung's %d", avg, top.maxrate)
	}
	f, err := os.Open(src)
	if err != nil {
		return false, err.Error()
	}
	defer f.Close()
	next := 0.0
	var window int64
	windowStart := 0.0
	for _, pk := range pkts {
		if pk.t >= next-1e-9 {
			if !pk.key {
				return false, fmt.Sprintf("no keyframe at %.3f s", pk.t)
			}
			if idr, err := isIDR(f, pk); err != nil || !idr {
				return false, fmt.Sprintf("keyframe at %.3f s is not an IDR (%v)", pk.t, err)
			}
			next += keyframeSeconds
			for pk.t >= next-1e-9 {
				next += keyframeSeconds
			}
		}
		if pk.t-windowStart >= segmentSeconds {
			if kbit := float64(window*8) / segmentSeconds / 1000; kbit > 2*float64(top.maxrate) {
				return false, fmt.Sprintf("%.0f kbit/s peak near %.0f s", kbit, windowStart)
			}
			window, windowStart = 0, pk.t
		}
		window += pk.size
	}
	return true, ""
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
		if err1 != nil || err2 != nil || err3 != nil {
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

// isIDR reports whether a length-prefixed (MP4) H.264 packet's first slice
// is an IDR slice.
func isIDR(f *os.File, pk sourcePacket) (bool, error) {
	b := make([]byte, pk.size)
	if _, err := f.ReadAt(b, pk.pos); err != nil {
		return false, err
	}
	for len(b) >= 5 {
		n := int(binary.BigEndian.Uint32(b))
		if n <= 0 || n > len(b)-4 {
			return false, fmt.Errorf("NAL length %d", n)
		}
		switch b[4] & 0x1f {
		case 5:
			return true, nil
		case 1, 2, 3, 4:
			return false, nil
		}
		b = b[4+n:]
	}
	return false, fmt.Errorf("no slice")
}

// copyRung stream-copies the source's video into rung n's single-file fMP4.
func copyRung(ctx context.Context, src, dir string, p plan, n int) error {
	args := append(append([]string{"-v", "error", "-nostdin"}, inputOptions(sourceDemuxers)...), "-i", src,
		"-map", fmt.Sprintf("0:%d", p.video), "-c", "copy", "-map_metadata", "-1")
	args = append(args, hlsArgs(filepath.Join(dir, fmt.Sprintf("v%d.mp4", n)), filepath.Join(dir, fmt.Sprintf("v%d.m3u8", n)))...)
	_, err := command(ctx, "ffmpeg", args...)
	return err
}

// sameSegments reports whether rungs a and b in dir have the same segment
// durations, so players switch between them at the same times.
func sameSegments(dir string, a, b int) bool {
	var pls [2]playlist
	for i, n := range []int{a, b} {
		v := filepath.Join(dir, fmt.Sprintf("v%d", n))
		st, err := os.Stat(v + ".mp4")
		if err != nil {
			return false
		}
		if pls[i], err = parsePlaylist(v+".m3u8", st.Size()); err != nil {
			return false
		}
	}
	if len(pls[0].segments) != len(pls[1].segments) {
		return false
	}
	for i, s := range pls[0].segments {
		if math.Abs(s.Seconds-pls[1].segments[i].Seconds) > 1e-3 {
			return false
		}
	}
	return true
}
