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
	"unicode/utf8"

	"github.com/google/uuid"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Manifest is an item's manifest.json: an ordered, app-defined virtual file
// system over the item's private blobs. Files is in an explicit,
// deterministic order: each Upload's files by name by default (reordered by
// commit ops), then each private preset's outputs in their uploads' order.
// Readers never re-sort it. Public publications belong to their source upload.
type Manifest struct {
	V                 int    `json:"v"`
	PublicationLayout int    `json:"publication_layout,omitempty"`
	Hidden            bool   `json:"hidden,omitempty"`      // legacy visibility flag; ignored by processing and public storage
	Incarnation       string `json:"incarnation,omitempty"` // changes only on explicit reset
	Deleted           bool   `json:"deleted,omitempty"`     // retained S3 tombstone; ordinary edits cannot recreate it
	// Full: a producer could not record its outputs within the bound, so
	// private outputs stop until a commit frees Deficit bytes (in the
	// manifest and in what its uploads would still add): how far past the
	// bound the refused record went.
	Full       bool                  `json:"full,omitempty"`
	Deficit    int64                 `json:"deficit,omitempty"`
	Meta       map[string]any        `json:"meta,omitempty"` // the app's template values, e.g. title
	Files      []media.File          `json:"files"`
	Deliveries []PublicationDelivery `json:"deliveries,omitempty"` // exact publication copies pinned until explicit withdrawal
	Receipt    *media.CommitReceipt  `json:"receipt,omitempty"`    // the last manifest attempt; retained until its journal effects settle

	index   map[string]int
	size    int64    // its JSON length when last read or written
	cleanup []string // worker scratch outputs, committed as exact journal effects
}

// SetPublication records a generation on its source upload: a reservation
// replaces an earlier successor and keeps the current generation, but in a
// Full manifest, which has no room for both, replaces it too; a ready
// generation replaces both.
func (m *Manifest) SetPublication(from string, p media.Publication) {
	if i := m.Find(from); i >= 0 {
		pubs := slices.DeleteFunc(slices.Clone(m.Files[i].Public), func(old media.Publication) bool {
			return old.Preset == p.Preset && (p.Ready() || !old.Ready() || m.Full)
		})
		m.Files[i].Public = append(pubs, p)
	}
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

// Outputs lists the derived files of preset from the upload (or zip
// prefix) from, in manifest order.
func (m *Manifest) Outputs(from, preset string) []media.File {
	var out []media.File
	for _, f := range m.Files {
		if f.Preset == preset && f.From == from {
			out = append(out, f)
		}
	}
	return out
}

// Blobs lists every blob the manifest references: kept uploads, derived
// files and track indexes.
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
		for _, p := range f.Public {
			out = append(out, p.Blobs...)
		}
	}
	for _, d := range m.Deliveries {
		if !d.Withdrawn {
			for _, f := range d.Files {
				if !d.Ready && !d.Adopted {
					out = append(out, f.Source)
				}
				out = append(out, f.Blob)
			}
		}
	}
	return out
}

// StagedNames lists the staged uploads the manifest references (in temp/).
func (m *Manifest) StagedNames() []string {
	var out []string
	for _, f := range m.Files {
		if f.Staged != "" {
			out = append(out, f.Staged)
		}
	}
	return out
}

// Validate requires unique paths, well-formed blobs and edits, and upload
// and derived fields where they belong.
func (m *Manifest) Validate() error {
	if m.Incarnation != "" {
		id, err := uuid.Parse(m.Incarnation)
		if err != nil || id == uuid.Nil || id.String() != m.Incarnation {
			return errors.New("media: invalid manifest incarnation")
		}
	}
	if m.Deleted && (!m.Hidden || len(m.Files) != 0 || len(m.Meta) != 0 || len(m.Deliveries) != 0 || m.Full || m.Deficit != 0) {
		return errors.New("media: deletion tombstone must be hidden and empty")
	}
	if m.Receipt != nil {
		for _, value := range []string{m.Receipt.Operation, m.Receipt.Attempt} {
			id, err := uuid.Parse(value)
			if err != nil || id == uuid.Nil || id.String() != value {
				return errors.New("media: invalid manifest commit receipt")
			}
		}
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
		case !f.IsUpload() && (f.Edit != nil || f.Frame != nil || f.Pending != nil || f.Unattached || len(f.Public) > 0 || f.Editor != nil):
			return fmt.Errorf("media: manifest file %q: a derived file has upload fields", f.Path)
		case f.IsUpload() && (f.From != "" || f.FP != "" || f.Track != nil):
			return fmt.Errorf("media: manifest file %q: an upload has provenance", f.Path)
		}
		seen[f.Path] = true
		type slot struct {
			preset string
			state  media.PublicationState
		}
		slots := make(map[slot]bool, len(f.Public))
		for _, p := range f.Public {
			id, err := uuid.Parse(p.Generation)
			generation := err == nil && id != uuid.Nil && id.String() == p.Generation || p.Generation == "" && p.Ready()
			current, _ := f.Current(p.Preset)
			if p.Preset == "" || slots[slot{p.Preset, p.State}] || p.Source != f.Key() || p.FP == "" || !generation ||
				!p.Ready() && current.Generation == p.Generation || len(p.Names) == 0 || len(p.Dims) != len(p.Names) || p.State > media.PublicationReady {
				return fmt.Errorf("media: manifest file %q: invalid public publication", f.Path)
			}
			slots[slot{p.Preset, p.State}] = true
			if len(p.Blobs) > 0 {
				if len(p.Blobs) != len(p.Names) {
					return errors.New("media: prepared public outputs do not match names")
				}
				for _, blob := range p.Blobs {
					area, _, valid := layout.SplitBlobRef(blob)
					if !valid || area != layout.AreaInternal && area != layout.AreaPublic && area != layout.AreaPrivate {
						return errors.New("media: invalid public preparation or delivery area")
					}
				}
			}
			for j, name := range p.NamesOnDisk() {
				if !layout.ValidSegment(p.Names[j]) || !layout.ValidPublicName(name) || p.Ready() && (p.Dims[j].W <= 0 || p.Dims[j].H <= 0) {
					return fmt.Errorf("media: manifest file %q: invalid public rendition %q", f.Path, name)
				}
			}
		}
		// Bounds are checked at commit and by the producers (a failure, not
		// a refused edit), so a measured size never wedges a worker's edit.
		if err := f.Edit.Check(0, 0); err != nil {
			return fmt.Errorf("media: manifest file %q: edit: %w", f.Path, err)
		}
	}
	return validateDeliveries(m.Deliveries)
}

// Clone deep-copies the manifest for an edit: a cached manifest is shared,
// so an edit never mutates it.
func (m *Manifest) Clone() *Manifest {
	out := *m
	out.cleanup = nil
	if m.Receipt != nil {
		r := *m.Receipt
		out.Receipt = &r
	}
	out.Meta = cloneMap(m.Meta)
	out.Deliveries = slices.Clone(m.Deliveries)
	for i := range out.Deliveries {
		out.Deliveries[i].Files = slices.Clone(out.Deliveries[i].Files)
	}
	out.Files = make([]media.File, len(m.Files))
	for i, f := range m.Files {
		f.Meta, f.Pending = cloneMap(f.Meta), slices.Clone(f.Pending)
		if f.Admission != nil {
			admission := *f.Admission
			f.Admission = &admission
		}
		if f.Editor != nil {
			editor := *f.Editor
			f.Editor = &editor
		}
		f.Public = slices.Clone(f.Public)
		for j := range f.Public {
			if f.Public[j].Stable != nil {
				stable := *f.Public[j].Stable
				f.Public[j].Stable = &stable
			}
			f.Public[j].Names = slices.Clone(f.Public[j].Names)
			f.Public[j].Blobs = slices.Clone(f.Public[j].Blobs)
			f.Public[j].Dims = slices.Clone(f.Public[j].Dims)
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
		out.Files[i] = f
	}
	out.index = nil
	return &out
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

// SetOutputs replaces preset's outputs from the upload (or zip prefix) from
// with outs (whose From and Preset it sets) and clears preset from the
// upload's Pending. A ZIP has one output set per preset, including when a
// deployment changes its input prefix. Paths taken by other files are an error.
func (m *Manifest) SetOutputs(from, preset string, outs []media.File) error {
	zip := strings.HasSuffix(from, "/")
	m.Files = slices.DeleteFunc(m.Files, func(f media.File) bool { return f.Preset == preset && (zip || f.From == from) })
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

const (
	editHeadroom = 4 << 10
)

var (
	// ErrManifestTooLarge: an edit refused because the manifest would grow
	// past its bound. A worker refused it marks the item Full (SetFull).
	ErrManifestTooLarge = errors.New("media: manifest edit refused: over its size limit")
)

var (
	// ErrManifestUnreadable: a stored manifest over MaxManifestBytes, written
	// before the bound; it does not decode.
	ErrManifestUnreadable = errors.New("media: stored manifest over its size limit")
)

// encodeManifest writes m as gzip JSON (HTML characters unescaped: a name of
// "<" costs one byte) and records its JSON length.
func encodeManifest(m *Manifest) ([]byte, error) {
	m.V = media.ManifestVersion
	if m.Files == nil {
		m.Files = []media.File{}
	}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	// A return to earlier JSON must not restore its ETag: an old If-Match
	// would become valid again. Keep the write nonce in gzip's extra header
	// so it does not consume JSON headroom reserved for flags on full items.
	revision := uuid.New()
	zw.Extra = revision[:]
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

// DecodeManifest reads a stored manifest (gzip JSON, or plain JSON).
func DecodeManifest(b []byte) (*Manifest, error) { return decodeManifest(b) }

// decodeManifest reads gzip JSON (or plain JSON), at most MaxManifestBytes
// of it (ErrManifestUnreadable past that), indexes it and records its JSON
// length.
func decodeManifest(b []byte) (*Manifest, error) {
	m, err := decodeStored(b)
	switch {
	case err != nil:
		return nil, err
	case m.V != media.ManifestVersion:
		return nil, fmt.Errorf("media: manifest version %d, want %d", m.V, media.ManifestVersion)
	}
	return m, nil
}

// decodeStored decodes a manifest of any version.
func decodeStored(b []byte) (*Manifest, error) {
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
	case r < 0x20 || r == '\u2028' || r == '\u2029' || r == utf8.RuneError:
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
