package media

import "cmp"

// Helpers the video producers (media/video) need from the core.

// Codec is a video codec of an HLS ladder: each rung is encoded in each of
// the worker's codecs (Track.Codec).
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecHEVC Codec = "hevc"
	CodecAV1  Codec = "av1"
)

// UploadOf is the Upload a file path belongs to.
func (k *Kind) UploadOf(path string) (Upload, bool) {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return Upload{}, false
	}
	return k.Uploads[i], true
}

// FramesVideo is the video upload the upload at path is grabbed from (its
// Upload.Frames), if the manifest has one.
func (k *Kind) FramesVideo(m *Manifest, path string) (File, bool) {
	u, ok := k.UploadOf(path)
	if !ok || u.Frames == "" {
		return File{}, false
	}
	for _, f := range m.Files {
		if stem, _ := splitExt(f.Path); f.IsUpload() && stem == u.Frames {
			return f, true
		}
	}
	return File{}, false
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
