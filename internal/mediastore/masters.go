package mediastore

import (
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Recipe is upload f's master recipe: its kind's for an image or video
// upload, the identity for any other (archives, audio, subtitles, frames
// grabbed from a video, which derive from that video's master).
func (k *RegisteredKind) Recipe(f media.File) *media.Recipe {
	if f.Frame == nil && (isImageType(f.Type) || isVideoType(f.Type)) {
		return k.Master
	}
	return nil
}

// MasterCurrent reports an upload whose master its kind's current recipe
// made from its current blob: its renditions may derive from it.
func (k *RegisteredKind) MasterCurrent(f media.File) bool {
	if r := k.Recipe(f); r.Identity() {
		return f.IsUpload() && f.Blob != "" // the original, recorded or not
	}
	if !f.IsUpload() || f.Blob == "" || f.Master == nil || f.Master.Recipe != k.Recipe(f).FP() {
		return false
	}
	if f.Master.IsOriginal() {
		return blobHash(f.Master.Blob) == blobHash(f.Blob)
	}
	return f.Master.Of == blobHash(f.Blob)
}

// MasterPending reports an upload whose master its recipe's hooks have yet
// to make: a new upload's, or one whose recipe changed. It keeps any master
// it has, and that master's renditions, until the new one is made.
func (k *RegisteredKind) MasterPending(f media.File) bool {
	return f.IsUpload() && f.Blob != "" && !f.Gone && f.Fail() == nil && !k.MasterCurrent(f)
}

// normalizeMaster makes an identity recipe's master the upload itself and
// drops a master made from another original. Hooks make the others.
func (k *RegisteredKind) normalizeMaster(f *media.File) {
	r := k.Recipe(*f)
	switch {
	case f.Blob == "":
		f.Master = nil
	case r.Identity():
		f.Master = &media.Master{Blob: f.Blob, Recipe: media.IdentityRecipe}
	case f.Master == nil:
	case f.Master.IsOriginal() && blobHash(f.Master.Blob) == blobHash(f.Blob):
		if f.Master.Blob != f.Blob {
			master := *f.Master
			master.Blob = f.Blob
			f.Master = &master
		}
	case f.Master.IsOriginal() || f.Master.Of != blobHash(f.Blob):
		f.Master = nil
	}
}

// SetMaster records upload path's master.
func (m *Manifest) SetMaster(path string, master media.Master) {
	if i := m.Find(path); i >= 0 {
		m.Files[i].Master = &master
	}
}

// Input is upload f as its renditions read it: its master's blob, type,
// size and pixel size, with f's edit scaled to the master's pixels.
func Input(f media.File) media.File {
	m := f.Master
	if m == nil || m.IsOriginal() {
		return f
	}
	in := f
	in.Blob, in.Size = m.Blob, m.Size
	if m.Type != "" {
		in.Type = m.Type
	}
	if m.W > 0 && m.H > 0 {
		in.Edit = scaleEdit(f.Edit, f.W, f.H, m.W, m.H)
		in.W, in.H = m.W, m.H
	}
	return in
}

// InputHash is the sha256 upload f's renditions derive from: its master's.
func InputHash(f media.File) string {
	if f.Master != nil {
		return blobHash(f.Master.Blob)
	}
	return blobHash(f.Blob)
}

func blobHash(ref string) string {
	_, h, _ := layout.SplitBlobRef(ref)
	return h
}

// scaleEdit maps e's crop from a w×h source to a mw×mh one.
func scaleEdit(e *media.Edit, w, h, mw, mh int) *media.Edit {
	if e == nil || e.Crop == nil || w <= 0 || h <= 0 || w == mw && h == mh {
		return e
	}
	out, c := *e, *e.Crop
	c.X, c.W = c.X*mw/w, c.W*mw/w
	c.Y, c.H = c.Y*mh/h, c.H*mh/h
	out.Crop = &c
	return &out
}
