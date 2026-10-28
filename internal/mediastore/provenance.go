package mediastore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"strings"

	media "github.com/open-rails/contentkit/media"
)

// Fingerprint hashes a derived file's inputs: for a producer, the source's
// blob, its edit, the preset's spec and the producer's recipe version. A
// derived file is stale exactly when its FP no longer matches.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// SpecFP is the fingerprint of a spec (any JSON value) applied to upload src
// by a producer at recipe.
func SpecFP(src media.File, spec any, recipe string) string {
	b, _ := json.Marshal(spec)
	return Fingerprint(src.Blob, src.Edit.Hash(), string(b), recipe)
}

// ZipInputs are the files a Zip preset packs: the files under its prefix
// whose upload is attached, in manifest order.
func (k *RegisteredKind) ZipInputs(m *Manifest, p *media.Private) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, p.Zip) && f.Blob != "" && !f.Gone && k.attached(m, f) {
			out = append(out, f)
		}
	}
	return out
}

// ZipFP is the fingerprint of a Zip preset's current inputs.
func ZipFP(inputs []media.File) string {
	parts := []string{media.ZipRecipe}
	for _, f := range inputs {
		parts = append(parts, f.Path, f.Blob)
	}
	return Fingerprint(parts...)
}

// LazyZip reports a reader download archive assembled on demand, rather than
// a processing output. Other Zip presets still build with their producer.
func (k *RegisteredKind) LazyZip(p *media.Private) bool {
	return p.Zip != "" && k.Publish != nil && k.Publish.Download == p.Name
}

// ZipStale reports a Zip preset whose output does not pack its current
// inputs: missing with inputs, kept without, or another fingerprint.
func (k *RegisteredKind) ZipStale(m *Manifest, p *media.Private) bool {
	in := k.ZipInputs(m, p)
	outs := m.Outputs(p.Zip, p.Name)
	if len(in) == 0 {
		return len(outs) > 0
	}
	return len(outs) != 1 || outs[0].FP != ZipFP(in)
}

// attached reports a file that is part of the item: an attached upload, or
// a derived file of one.
func (k *RegisteredKind) attached(m *Manifest, f media.File) bool {
	if f.IsUpload() {
		return !f.Unattached && media.PageIncluded(f)
	}
	if src, ok := m.Get(f.From); ok && src.IsUpload() {
		return !src.Unattached && media.PageIncluded(src)
	}
	return true
}

// Readiness is the manifest's readiness under k.
func (k *RegisteredKind) Readiness(m *Manifest) media.Readiness {
	return k.readiness(m, false)
}

// NeedsProcessing includes staged uploads, whose private renditions can be
// prepared before Save. Public renditions still wait for attachment.
func (k *RegisteredKind) NeedsProcessing(m *Manifest) bool {
	return len(k.readiness(m, true).Processing) != 0
}

func (k *RegisteredKind) readiness(m *Manifest, includeUnattached bool) media.Readiness {
	var r media.Readiness
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached && !includeUnattached {
			continue
		}
		public := !f.Unattached
		measure := includeUnattached && isImageType(f.Type) && (f.W <= 0 || f.H <= 0) && len(k.Presets(f.Path, false)) != 0
		switch {
		case f.Fail() != nil && !k.audioOriginal(f):
			r.Failed = append(r.Failed, f.Path)
		case f.Blob == "" || measure || slices.ContainsFunc(f.Pending, func(p string) bool { return k.private(p) != nil || public && k.public(p) != nil }) ||
			public && slices.ContainsFunc(f.Public, func(p media.Publication) bool { return !p.Ready() }):
			r.Processing = append(r.Processing, f.Path)
		}
	}
	for i := range k.Private {
		if p := &k.Private[i]; p.Zip != "" && !k.LazyZip(p) && k.ZipStale(m, p) {
			r.Processing = append(r.Processing, p.To)
		}
	}
	switch {
	case m.Full && len(r.Processing) > 0:
		r.State = media.StateFull
	case len(r.Processing) > 0:
		r.State = media.StateProcessing
	case len(r.Failed) > 0:
		r.State = media.StateFailed
	default:
		r.State = media.StateReady
	}
	return r
}

// Normalize brings m to its canonical form under k after an edit: the
// file order, the download names, and dropped originals (Gone).
func (k *RegisteredKind) Normalize(m *Manifest) {
	k.order(m)
	vars := metaVars(m.Meta)
	for i := range m.Files {
		f := &m.Files[i]
		if f.IsUpload() {
			// A deployment can remove a preset. It must not leave the upload
			// permanently processing for work no current producer can perform.
			f.Pending = slices.DeleteFunc(slices.Clone(f.Pending), func(name string) bool {
				return k.private(name) == nil && k.public(name) == nil
			})
			if len(f.Pending) == 0 {
				f.Pending = nil
			}
			f.Public = slices.DeleteFunc(slices.Clone(f.Public), func(pub media.Publication) bool {
				p := k.public(pub.Preset)
				return p == nil || !k.owned(m, *f, p, pub)
			})
			if len(f.Public) == 0 {
				f.Public = nil
			}
			f.Gone = f.Gone || k.droppable(m, *f)
			continue
		}
		f.Download = ""
		if p := k.private(f.Preset); p != nil && p.Download != "" {
			vars["name"] = k.NameOf(f.From)
			if f.Download = CleanName(fill(p.Download, vars)); f.Download == "" {
				f.Download = path.Base(f.Path)
			}
		}
	}
	m.index = nil
}

// droppable reports an upload whose blob is no longer needed: its kind does
// not keep originals, it feeds private presets but no public preset or
// frame, it is no body image (new versions copy those, editors view them),
// and nothing is pending or failed from it.
func (k *RegisteredKind) droppable(m *Manifest, f media.File) bool {
	if k.Versions || k.KeepOriginals || k.IsPageUpload(f) || k.audioOriginal(f) || f.Blob == "" || len(f.Pending) > 0 || f.Fail() != nil || len(k.PublicFor(f.Path)) > 0 {
		return false
	}
	i, _, _, _, ok := k.upload(f.Path)
	if !ok || k.Uploads[i].Body || slices.ContainsFunc(k.Uploads, func(u media.Upload) bool { return u.Frames == k.Uploads[i].Path }) {
		return false
	}
	return len(k.PrivateFor(f.Path)) > 0
}

// order sorts m.Files canonically: each Upload's files in declaration
// order (attached before unattached, otherwise as they are), then each
// private preset's outputs in their uploads' order, then unknown files.
func (k *RegisteredKind) order(m *Manifest) {
	unknown := 2*len(k.Uploads) + len(k.Private)
	ranks := make(map[string]int, len(m.Files))
	var uploads []media.File
	for _, f := range m.Files {
		ranks[f.Path] = k.rank(f, unknown)
		if f.IsUpload() {
			uploads = append(uploads, f)
		}
	}
	slices.SortStableFunc(uploads, func(a, b media.File) int { return ranks[a.Path] - ranks[b.Path] })
	pos := make(map[string]int, len(uploads))
	for i, f := range uploads {
		pos[f.Path] = i
	}
	key := func(f media.File) int {
		if f.IsUpload() {
			return pos[f.Path]
		}
		if p, ok := pos[f.From]; ok {
			return p
		}
		return -1
	}
	slices.SortStableFunc(m.Files, func(a, b media.File) int {
		if ra, rb := ranks[a.Path], ranks[b.Path]; ra != rb {
			return ra - rb
		}
		return key(a) - key(b)
	})
	m.index = nil
}

func (k *RegisteredKind) rank(f media.File, unknown int) int {
	if f.IsUpload() {
		if u, _, _, _, ok := k.upload(f.Path); ok {
			if f.Unattached {
				return 2*u + 1
			}
			return 2 * u
		}
	} else if p := slices.IndexFunc(k.Private, func(p media.Private) bool { return p.Name == f.Preset }); p >= 0 {
		return 2*len(k.Uploads) + p
	}
	return unknown
}
