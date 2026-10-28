package mediastore

import (
	media "github.com/open-rails/contentkit/media"
)

// UploadOf is the Upload a file path belongs to.
func (k *RegisteredKind) UploadOf(path string) (media.Upload, bool) {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return media.Upload{}, false
	}
	return k.Uploads[i], true
}

// FramesVideo is the video upload the upload at path is grabbed from (its
// Upload.Frames), if the manifest has one.
func (k *RegisteredKind) FramesVideo(m *Manifest, path string) (media.File, bool) {
	u, ok := k.UploadOf(path)
	if !ok || u.Frames == "" {
		return media.File{}, false
	}
	for _, f := range m.Files {
		if stem, _ := splitExt(f.Path); f.IsUpload() && stem == u.Frames {
			return f, true
		}
	}
	return media.File{}, false
}
