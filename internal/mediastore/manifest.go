package mediastore

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Manifest is an item's manifest.json, the only record of its files: an
// ordered, app-defined virtual file system over hash-named objects. Files is
// in an explicit, deterministic order: each Upload's files by name by
// default (reordered by commit ops), then each preset's outputs in their
// uploads' order. Readers never re-sort it. It is rewritten in place with a
// conditional PUT (Manifests.Edit); Rev and Op let a writer recognise its own
// write and caches move only forward.
type Manifest struct {
	V         int            `json:"v"`
	Rev       int64          `json:"rev"`
	Op        string         `json:"op,omitempty"`
	Item      *ItemIdentity  `json:"item,omitempty"`
	WrittenAt time.Time      `json:"written_at,omitzero"`
	Deleted   *Deletion      `json:"deleted,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"` // the app's template values, e.g. title
	Files     []media.File   `json:"files"`
	// Retired lists objects no file references any more, until garbage
	// collection has deleted them: the undo window and the GC's work list.
	Retired []Retired `json:"retired,omitempty"`
	// Ops are the latest commits' operation ids with their requests'
	// digests: a retried commit is recognised after later writes, and an
	// id reused for another request is refused.
	Ops []OpReceipt `json:"ops,omitempty"`

	index map[string]int
	size  int64 // its JSON length when last read or written
}

// OpReceipt is a commit's operation id and its request's digest.
type OpReceipt struct {
	ID  string `json:"id"`
	Sum string `json:"sum"`
}

// maxReceipts bounds Manifest.Ops.
const maxReceipts = 32

// ItemIdentity names the item a manifest belongs to, so a manifest alone
// rebuilds its catalog cache.
type ItemIdentity struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	Version string `json:"version,omitempty"`
}

// Deletion marks a soft-deleted item (its files kept, the fence written) or
// an erased one (no files left; a tombstone that refuses late writers).
type Deletion struct {
	At     time.Time `json:"at"`
	Erased bool      `json:"erased,omitempty"`
}

// Retired is an object no file references. Due is when GC may delete it;
// Deleting, once set, fences re-reference of the hash until the delete has
// settled (TSettle).
type Retired struct {
	Blob     string     `json:"blob"`
	Path     string     `json:"path,omitempty"`
	At       time.Time  `json:"at"`
	Due      time.Time  `json:"due"`
	Deleting *time.Time `json:"deleting,omitempty"`
}

// NewFailure records err for upload f: an ImageError keeps its code and
// details; the message is capped at maxFailureBytes.
func NewFailure(f media.File, err error) *media.Failure {
	out := &media.Failure{Of: f.Key(), Message: err.Error()}
	if ie := AsImageError(err); ie != nil {
		out.Message, out.Code, out.Details = ie.Message, ie.Code, &ie.Details
	}
	// A tool's diagnostics can run to kilobytes: the manifest keeps the gist.
	for n := 0; jsonLen(out.Message) > maxFailureBytes; n++ {
		out.Message = strings.ToValidUTF8(out.Message[:min(len(out.Message), maxFailureBytes-n)], "")
	}
	return out
}

// maxFailureBytes caps a Failure's message in the manifest's JSON bytes.
const maxFailureBytes = 300

// editHeadroom is what optional records (page analysis) leave free below
// MaxManifestBytes for the outputs and failures still to be recorded.
const editHeadroom = 4 << 10

// Find returns the index of the file at path, or -1.
func (m *Manifest) Find(path string) int {
	i, ok := m.index[path]
	if ok && i < len(m.Files) && m.Files[i].Path == path {
		return i
	}
	if !ok && len(m.index) == len(m.Files) && m.index != nil {
		return -1
	}
	m.reindex()
	if i, ok := m.index[path]; ok {
		return i
	}
	return -1
}

// Get returns the file at path.
func (m *Manifest) Get(path string) (media.File, bool) {
	if i := m.Find(path); i >= 0 {
		return m.Files[i], true
	}
	return media.File{}, false
}

func (m *Manifest) reindex() {
	m.index = make(map[string]int, len(m.Files))
	for i, f := range m.Files {
		m.index[f.Path] = i
	}
}

// Outputs lists the derived files of preset from the upload from, in
// manifest order.
func (m *Manifest) Outputs(from, preset string) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if f.Preset == preset && f.From == from {
			out = append(out, f)
		}
	}
	return out
}

// Blobs lists every object the manifest's files reference: kept uploads,
// derived files, track indexes and editor views.
func (m *Manifest) Blobs() []string {
	var out []string
	for _, f := range m.Files {
		if f.Blob != "" && !f.Gone {
			out = append(out, f.Blob)
		}
		if f.Track != nil && f.Track.Index != "" {
			out = append(out, f.Track.Index)
		}
		if f.Editor != nil && !f.Gone {
			out = append(out, f.Editor.Blob)
		}
	}
	return out
}

// referenced is the set of Blobs.
func (m *Manifest) referenced() map[string]bool {
	out := map[string]bool{}
	for _, b := range m.Blobs() {
		out[b] = true
	}
	return out
}

// StagedNames lists the staged uploads the manifest references (in tmp/).
func (m *Manifest) StagedNames() []string {
	var out []string
	for _, f := range m.Files {
		if f.Staged != "" {
			out = append(out, f.Staged)
		}
	}
	return out
}

// Retiring returns the retired entry of blob, if any.
func (m *Manifest) Retiring(blob string) (Retired, bool) {
	for _, r := range m.Retired {
		if r.Blob == blob {
			return r, true
		}
	}
	return Retired{}, false
}

// ErrInvalidManifest wraps every Validate refusal: the stored manifest or the
// edit is malformed, so repeating the same work cannot succeed.
var ErrInvalidManifest = errors.New("media: invalid manifest")

// Validate requires unique paths, well-formed blobs and edits, and upload
// and derived fields where they belong.
func (m *Manifest) Validate() error {
	if err := m.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	return nil
}

func (m *Manifest) validate() error {
	if m.Deleted != nil && m.Deleted.Erased && (len(m.Files) != 0 || len(m.Meta) != 0) {
		return errors.New("media: an erased item's tombstone must be empty")
	}
	seen := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		switch {
		case f.Path == "" || seen[f.Path]:
			return fmt.Errorf("media: manifest file %d: empty or duplicate path %q", i, f.Path)
		case f.Staged != "" && (!layout.ValidStagedName(f.Staged) || f.Blob != "" || f.Frame != nil || !f.IsUpload()):
			return fmt.Errorf("media: manifest file %q: invalid staged upload %q", f.Path, f.Staged)
		case f.Blob != "" && !layout.ValidBlobRef(f.Blob), f.Blob == "" && f.Staged == "" && (f.Frame == nil || !f.IsUpload()):
			return fmt.Errorf("media: manifest file %q: invalid blob %q", f.Path, f.Blob)
		case f.Track != nil && f.Track.Index != "" && !layout.ValidBlobRef(f.Track.Index):
			return fmt.Errorf("media: manifest file %q: invalid track index %q", f.Path, f.Track.Index)
		case f.Editor != nil && (!layout.ValidBlobRef(f.Editor.Blob) || f.Editor.FP == ""):
			return fmt.Errorf("media: manifest file %q: invalid editor view", f.Path)
		case !f.IsUpload() && (f.Edit != nil || f.Frame != nil || f.Pending != nil || f.Unattached || f.Editor != nil || f.From == ""):
			return fmt.Errorf("media: manifest file %q: a derived file has upload fields", f.Path)
		case f.IsUpload() && (f.From != "" || f.FP != "" || f.Track != nil):
			return fmt.Errorf("media: manifest file %q: an upload has provenance", f.Path)
		}
		seen[f.Path] = true
		// Bounds are checked at commit and by the producers (a failure, not
		// a refused edit), so a measured size never wedges a worker's edit.
		if err := f.Edit.Check(0, 0); err != nil {
			return fmt.Errorf("media: manifest file %q: edit: %w", f.Path, err)
		}
	}
	retired := make(map[string]bool, len(m.Retired))
	for _, r := range m.Retired {
		if !layout.ValidBlobRef(r.Blob) || retired[r.Blob] || r.At.IsZero() || r.Due.IsZero() {
			return fmt.Errorf("media: invalid retired entry %q", r.Blob)
		}
		retired[r.Blob] = true
	}
	return nil
}

// Clone deep-copies the manifest for an edit: a cached manifest is shared,
// so an edit never mutates it.
func (m *Manifest) Clone() *Manifest {
	out := *m
	if m.Item != nil {
		item := *m.Item
		out.Item = &item
	}
	if m.Deleted != nil {
		d := *m.Deleted
		out.Deleted = &d
	}
	out.Meta = cloneMap(m.Meta)
	out.Retired = slices.Clone(m.Retired)
	out.Ops = slices.Clone(m.Ops)
	out.Files = make([]media.File, len(m.Files))
	for i, f := range m.Files {
		out.Files[i] = cloneFile(f)
	}
	out.index = nil
	return &out
}

func cloneFile(f media.File) media.File {
	f.Meta, f.Pending = cloneMap(f.Meta), slices.Clone(f.Pending)
	if f.Admission != nil {
		admission := *f.Admission
		f.Admission = &admission
	}
	if f.Editor != nil {
		editor := *f.Editor
		f.Editor = &editor
	}
	if f.Edit != nil {
		e := *f.Edit
		if e.Crop != nil {
			c := *e.Crop
			e.Crop = &c
		}
		f.Edit = &e
	}
	if f.Frame != nil {
		x := *f.Frame
		if x.From != nil {
			from := *x.From
			x.From = &from
		}
		f.Frame = &x
	}
	if f.Failed != nil {
		x := *f.Failed
		if x.Details != nil {
			d := *x.Details
			d.Allowed = slices.Clone(d.Allowed)
			x.Details = &d
		}
		f.Failed = &x
	}
	if f.Track != nil {
		x := *f.Track
		f.Track = &x
	}
	return f
}

// cloneMap deep-copies JSON-shaped values.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return cloneMap(v)
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = cloneValue(x)
		}
		return out
	}
	return v
}

// SetOutputs replaces preset's outputs from the upload from with outs (whose
// From and Preset it sets) and clears preset from the upload's Pending.
// Paths taken by other files are an error.
func (m *Manifest) SetOutputs(from, preset string, outs []media.File) error {
	m.Files = slices.DeleteFunc(m.Files, func(f media.File) bool { return f.Preset == preset && f.From == from })
	m.index = nil
	for _, o := range outs {
		if m.Find(o.Path) >= 0 {
			return fmt.Errorf("media: output %q of %s from %q: path taken", o.Path, preset, from)
		}
		o.From, o.Preset = from, preset
		m.Files = append(m.Files, o)
		m.index[o.Path] = len(m.Files) - 1
	}
	m.ClearPending(from, preset)
	return nil
}

// ClearPending removes preset from the Pending of the upload at path.
func (m *Manifest) ClearPending(path, preset string) {
	if i := m.Find(path); i >= 0 && slices.Contains(m.Files[i].Pending, preset) {
		m.Files[i].Pending = slices.DeleteFunc(slices.Clone(m.Files[i].Pending), func(p string) bool { return p == preset })
		if len(m.Files[i].Pending) == 0 {
			m.Files[i].Pending = nil
		}
	}
}

// AddPending adds presets to the Pending of the upload at path.
func (m *Manifest) AddPending(path string, presets ...string) {
	i := m.Find(path)
	if i < 0 {
		return
	}
	p := slices.Clone(m.Files[i].Pending)
	for _, name := range presets {
		if !slices.Contains(p, name) {
			p = append(p, name)
		}
	}
	m.Files[i].Pending = p
}

// SetFailed records err on the upload at path for its current blob and edit,
// and clears its Pending.
func (m *Manifest) SetFailed(path string, err error) {
	if i := m.Find(path); i >= 0 {
		m.Files[i].Failed = NewFailure(m.Files[i], err)
		m.Files[i].Pending = nil
	}
}

var (
	// ErrManifestTooLarge: an edit refused because the manifest would grow
	// past MaxManifestBytes.
	ErrManifestTooLarge = errors.New("media: manifest edit refused: over its size limit")
)

var (
	// ErrManifestUnreadable: a stored manifest over MaxManifestBytes; it does
	// not decode.
	ErrManifestUnreadable = errors.New("media: stored manifest over its size limit")
)

// encodeManifest writes m as gzip JSON (HTML characters unescaped: a name of
// "<" costs one byte) and records its JSON length. Rev changes on every
// write, so no two revisions share an ETag.
func encodeManifest(m *Manifest) ([]byte, error) {
	m.V = media.ManifestVersion
	if m.Files == nil {
		m.Files = []media.File{}
	}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	cw := &countWriter{w: zw}
	enc := json.NewEncoder(cw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	m.size = cw.n
	return b.Bytes(), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// EncodeManifest is the stored form of m.
func EncodeManifest(m *Manifest) ([]byte, error) { return encodeManifest(m) }

// DecodeManifest reads a stored manifest (gzip JSON, or plain JSON).
func DecodeManifest(b []byte) (*Manifest, error) { return decodeManifest(b) }

// decodeManifest reads gzip JSON (or plain JSON), at most MaxManifestBytes
// of it (ErrManifestUnreadable past that), indexes it and records its JSON
// length. Only the current version decodes.
func decodeManifest(b []byte) (*Manifest, error) {
	r := io.Reader(bytes.NewReader(b))
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	lr := &io.LimitedReader{R: r, N: media.MaxManifestBytes + 1}
	var m Manifest
	err := json.NewDecoder(lr).Decode(&m)
	if lr.N <= 0 {
		return nil, ErrManifestUnreadable
	} else if err != nil {
		return nil, err
	}
	if m.V != media.ManifestVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrInvalidManifest, m.V, media.ManifestVersion)
	}
	m.reindex()
	m.size = media.MaxManifestBytes + 1 - lr.N
	return &m, nil
}

// jsonLen is s's length as a JSON string's contents, as encodeManifest
// writes it.
func jsonLen(s string) int {
	n := 0
	for _, r := range s {
		n += jsonRuneLen(r)
	}
	return n
}

// jsonRuneLen is r's length in a JSON string: quotes and backslashes and
// the short escapes doubled, other control characters, invalid bytes and
// the line separators U+2028 and U+2029 as \uXXXX.
func jsonRuneLen(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == ' ' || r == ' ' || r == utf8.RuneError:
		return 6
	}
	return utf8.RuneLen(r)
}

// metaBytes is meta's JSON length.
func metaBytes(meta map[string]any) int {
	if len(meta) == 0 {
		return 0
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return media.MaxItemMetaBytes + 1
	}
	return len(b)
}
