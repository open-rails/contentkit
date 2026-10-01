package media

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
