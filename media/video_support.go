package media

import (
	"cmp"
)

// Codec is a video codec of an HLS ladder: each rung is encoded in each of
// the worker's codecs (Track.Codec).
type Codec string

const (
	CodecH264 Codec = "h264"
)

const (
	CodecAV1 Codec = "av1"
)

// DynamicRange is a video's: SDR, or HDR with the PQ (HDR10) or HLG
// transfer. AV1 renditions keep an HDR source's range; its H.264
// renditions, sprite and frames are tone-mapped to BT.709 SDR.
type DynamicRange string

const (
	RangeSDR DynamicRange = ""
	RangePQ  DynamicRange = "pq"  // SMPTE ST 2084
	RangeHLG DynamicRange = "hlg" // ARIB STD-B67
)

// HLS is the range as an HLS VIDEO-RANGE value.
func (r DynamicRange) HLS() string {
	switch r {
	case RangePQ:
		return "PQ"
	case RangeHLG:
		return "HLG"
	}
	return "SDR"
}

// DefaultVideoLimits are an upload's VideoLimits where it sets none: 4 h at
// up to 60 fps, frames up to DCI 8K (8192×4320; the ladder scales larger
// sources down to 4K), and the work of a 4 h 4K60 default ladder (2160,
// 1080, 480) in three codecs plus MP4 downloads, about 3e13 pixel-frames.
var DefaultVideoLimits = VideoLimits{MaxSeconds: 4 * 3600, MaxFPS: 60, MaxPixels: 8192 * 4320, MaxWork: 4e13}

// Limits are the limits in effect: l's, and the defaults for nil or zero fields.
func (l *VideoLimits) Limits() VideoLimits {
	d := DefaultVideoLimits
	if l == nil {
		return d
	}
	return VideoLimits{MaxSeconds: cmp.Or(l.MaxSeconds, d.MaxSeconds), MaxFPS: cmp.Or(l.MaxFPS, d.MaxFPS),
		MaxPixels: cmp.Or(l.MaxPixels, d.MaxPixels), MaxWork: cmp.Or(l.MaxWork, d.MaxWork)}
}
