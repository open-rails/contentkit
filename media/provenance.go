package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"strings"
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
func SpecFP(src File, spec any, recipe string) string {
	b, _ := json.Marshal(spec)
	return Fingerprint(src.Blob, src.Edit.Hash(), string(b), recipe)
}

// ZipRecipe versions the Zip producer.
const ZipRecipe = "zip-store-1"

// ZipInputs are the files a Zip preset packs: the files under its prefix
// whose upload is attached, in manifest order.
func (k *Kind) ZipInputs(m *Manifest, p *Private) []File {
	var out []File
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, p.Zip) && f.Blob != "" && !f.Gone && k.attached(m, f) {
			out = append(out, f)
		}
	}
	return out
}

// ZipFP is the fingerprint of a Zip preset's current inputs.
func ZipFP(inputs []File) string {
	parts := []string{ZipRecipe}
	for _, f := range inputs {
		parts = append(parts, f.Path, f.Blob)
	}
	return Fingerprint(parts...)
}

// ZipStale reports a Zip preset whose output does not pack its current
// inputs: missing with inputs, kept without, or another fingerprint.
func (k *Kind) ZipStale(m *Manifest, p *Private) bool {
	in := k.ZipInputs(m, p)
	outs := m.Outputs(p.Zip, p.Name)
	if len(in) == 0 {
		return len(outs) > 0
	}
	return len(outs) != 1 || outs[0].FP != ZipFP(in)
}

// attached reports a file that is part of the item: an attached upload, or
// a derived file of one.
func (k *Kind) attached(m *Manifest, f File) bool {
	if f.IsUpload() {
		return !f.Unattached
	}
	if src, ok := m.Get(f.From); ok && src.IsUpload() {
		return !src.Unattached
	}
	return true
}

// Processing states of an item.
const (
	StateReady      = "ready"
	StateProcessing = "processing"
	StateFailed     = "failed"
	StateFull       = "full" // the manifest is Full: work is left, stopped until a commit shrinks it
)

// Readiness is whether an item's media is processed: ready when no attached
// upload is pending (its public presets count only while the item is
// visible) and every zip packs its inputs; processing while any is; failed
// once nothing is processing and an upload failed for its current blob and
// edit; full while work is left on a Full manifest. Processing and Failed
// name upload paths (and zip outputs).
type Readiness struct {
	State      string   `json:"state"`
	Processing []string `json:"processing,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

func (r Readiness) Ready() bool { return r.State == StateReady }

// Readiness is the manifest's readiness under k.
func (k *Kind) Readiness(m *Manifest) Readiness {
	var r Readiness
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached {
			continue
		}
		switch {
		case f.Fail() != nil:
			r.Failed = append(r.Failed, f.Path)
		case f.Blob == "" || slices.ContainsFunc(f.Pending, func(p string) bool { return !m.Hidden || k.public(p) == nil }):
			r.Processing = append(r.Processing, f.Path)
		}
	}
	for i := range k.Private {
		if p := &k.Private[i]; p.Zip != "" && k.ZipStale(m, p) {
			r.Processing = append(r.Processing, p.To)
		}
	}
	switch {
	case m.Full && len(r.Processing) > 0:
		r.State = StateFull
	case len(r.Processing) > 0:
		r.State = StateProcessing
	case len(r.Failed) > 0:
		r.State = StateFailed
	default:
		r.State = StateReady
	}
	return r
}

// Normalize brings m to its canonical form under k after an edit: the
// file order, the download names, and dropped originals (Gone).
func (k *Kind) Normalize(m *Manifest) {
	k.order(m)
	vars := metaVars(m.Meta)
	for i := range m.Files {
		f := &m.Files[i]
		if f.IsUpload() {
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
// frame, and nothing is pending or failed from it.
func (k *Kind) droppable(m *Manifest, f File) bool {
	if k.KeepOriginals || f.Blob == "" || len(f.Pending) > 0 || f.Fail() != nil || len(k.PublicFor(f.Path)) > 0 {
		return false
	}
	i, _, _, _, ok := k.upload(f.Path)
	if !ok || slices.ContainsFunc(k.Uploads, func(u Upload) bool { return u.Frames == k.Uploads[i].Path }) {
		return false
	}
	return len(k.PrivateFor(f.Path)) > 0
}

// order sorts m.Files canonically: each Upload's files in declaration
// order (attached before unattached, otherwise as they are), then each
// private preset's outputs in their uploads' order, then unknown files.
func (k *Kind) order(m *Manifest) {
	unknown := 2*len(k.Uploads) + len(k.Private)
	ranks := make(map[string]int, len(m.Files))
	var uploads []File
	for _, f := range m.Files {
		ranks[f.Path] = k.rank(f, unknown)
		if f.IsUpload() {
			uploads = append(uploads, f)
		}
	}
	slices.SortStableFunc(uploads, func(a, b File) int { return ranks[a.Path] - ranks[b.Path] })
	pos := make(map[string]int, len(uploads))
	for i, f := range uploads {
		pos[f.Path] = i
	}
	key := func(f File) int {
		if f.IsUpload() {
			return pos[f.Path]
		}
		if p, ok := pos[f.From]; ok {
			return p
		}
		return -1
	}
	slices.SortStableFunc(m.Files, func(a, b File) int {
		if ra, rb := ranks[a.Path], ranks[b.Path]; ra != rb {
			return ra - rb
		}
		return key(a) - key(b)
	})
	m.index = nil
}

func (k *Kind) rank(f File, unknown int) int {
	if f.IsUpload() {
		if u, _, _, _, ok := k.upload(f.Path); ok {
			if f.Unattached {
				return 2*u + 1
			}
			return 2 * u
		}
	} else if p := slices.IndexFunc(k.Private, func(p Private) bool { return p.Name == f.Preset }); p >= 0 {
		return 2*len(k.Uploads) + p
	}
	return unknown
}
