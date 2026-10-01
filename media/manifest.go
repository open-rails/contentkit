package media

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/open-rails/contentkit/media/layout"
)

// ManifestVersion is the manifest format.
const ManifestVersion = 2

// Manifest is an item's manifest.json: an ordered, app-defined virtual file
// system over the item's private blobs. Files is in an explicit,
// deterministic order: each Upload's files by name by default (reordered by
// commit ops), then each private preset's outputs in their uploads' order.
// Readers never re-sort it. Public files are never listed.
type Manifest struct {
	V      int            `json:"v"`
	Hidden bool           `json:"hidden,omitempty"` // set by Expose; public files are then absent
	Meta   map[string]any `json:"meta,omitempty"`   // the app's template values, e.g. title
	Files  []File         `json:"files"`

	index map[string]int
}

// File is one file: an upload (no Preset) or a derived file.
type File struct {
	Path string  `json:"path"`           // app path: "originals/001.png", "low-res/001.webp"
	Blob string  `json:"blob,omitempty"` // "sha256-{hex}" in private/; "" for a staged upload or a frame not grabbed yet
	Type string  `json:"type"`
	Size int64   `json:"size,omitempty"`
	W    int     `json:"w,omitempty"` // an upload's oriented size once measured; an output's size
	H    int     `json:"h,omitempty"`
	Dur  float64 `json:"dur,omitempty"`

	// Uploads:
	Staged     string         `json:"staged,omitempty"`     // "u-{uuid}" in temp/ until the worker hashes and places it at Blob
	CreateID   string         `json:"create_id,omitempty"`  // create-only put receipt; retained through placement and processing
	Edit       *Edit          `json:"edit,omitempty"`       // crop and rotate in source pixels
	Frame      *Frame         `json:"frame,omitempty"`      // grabbed from the Upload.Frames video
	Meta       map[string]any `json:"meta,omitempty"`       // teaser, lang, label, …
	Unattached bool           `json:"unattached,omitempty"` // processed on upload, not yet part of the item
	Gone       bool           `json:"gone,omitempty"`       // blob dropped (KeepOriginals false); the hash stays for provenance
	Pending    []string       `json:"pending,omitempty"`    // presets still producing from this upload
	Failed     *Failure       `json:"failed,omitempty"`     // this blob and edit cannot be processed

	// Derived files:
	From     string `json:"from,omitempty"`     // the upload's path, or a zip's prefix
	Preset   string `json:"preset,omitempty"`   // the Private preset
	FP       string `json:"fp,omitempty"`       // Fingerprint of its inputs
	Download string `json:"download,omitempty"` // the human download name
	Track    *Track `json:"track,omitempty"`    // HLS
}

// Frame is an upload grabbed from a frame of its Upload.Frames video at T
// seconds; Auto lets the worker choose T. Of is the video blob it was
// grabbed from: a new video grabs again.
type Frame struct {
	T    float64 `json:"t,omitempty"`
	Auto bool    `json:"auto,omitempty"`
	Of   string  `json:"of,omitempty"`
}

// Failure is why an upload's blob through its edit cannot be processed.
type Failure struct {
	Of      string        `json:"of"` // File.Key it was recorded for
	Message string        `json:"message"`
	Code    string        `json:"code,omitempty"` // an ImageError's code
	Details *ErrorDetails `json:"details,omitempty"`
}

// Track is an HLS track file. It is small: the segment table or sprite grid
// is its own private blob, Index (a TrackIndex), cached by hash.
type Track struct {
	Kind      string `json:"kind"`             // TrackVideo, TrackAudio, TrackSubs, TrackSprite
	Codec     string `json:"codec,omitempty"`  // video: h264, hevc, av1
	Codecs    string `json:"codecs,omitempty"` // RFC 6381
	Bandwidth int    `json:"bw,omitempty"`
	Average   int    `json:"avg,omitempty"`
	ID        string `json:"id,omitempty"`
	Lang      string `json:"lang,omitempty"`
	Label     string `json:"label,omitempty"`
	Default   bool   `json:"default,omitempty"`
	Forced    bool   `json:"forced,omitempty"`
	Index     string `json:"index,omitempty"` // "sha256-{hex}": the TrackIndex
}

// Track kinds.
const (
	TrackVideo  = "video"
	TrackAudio  = "audio"
	TrackSubs   = "subs"
	TrackSprite = "sprite"
)

// TrackIndex is a Track's index blob: a byte-range track's segments (its
// init segment is bytes [0, Segments[0].Offset)), or a sprite's grid.
type TrackIndex struct {
	Segments []Segment `json:"segments,omitempty"`
	Sprite   *Sprite   `json:"sprite,omitempty"`
}

// Sprite is a seek-preview grid of Cols×Rows tiles of W×H, one per Interval seconds.
type Sprite struct {
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	W        int     `json:"w"`
	H        int     `json:"h"`
	Interval float64 `json:"interval"`
}

// Segment is one EXT-X-BYTERANGE segment, encoded as [offset, length, seconds].
type Segment struct {
	Offset  int64
	Length  int64
	Seconds float64
}

func (s Segment) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]any{s.Offset, s.Length, s.Seconds})
}

func (s *Segment) UnmarshalJSON(b []byte) error {
	var v [3]json.Number
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("media: segment: %w", err)
	}
	var err error
	if s.Offset, err = v[0].Int64(); err == nil {
		if s.Length, err = v[1].Int64(); err == nil {
			s.Seconds, err = v[2].Float64()
		}
	}
	return err
}

// IsUpload reports an upload (a file with no preset).
func (f File) IsUpload() bool { return f.Preset == "" }

// Source is the object holding an upload's bytes: its blob, or its staged
// upload until placed.
func (f File) Source() string { return cmpOr(f.Blob, f.Staged) }

// Key identifies the source and edit an upload's Failure applies to.
func (f File) Key() string { return f.Source() + "." + f.Edit.Hash() }

// Fail is the upload's failure for its current blob and edit, or nil.
func (f File) Fail() *Failure {
	if f.Failed != nil && f.Failed.Of == f.Key() {
		return f.Failed
	}
	return nil
}

// Teaser reports meta.teaser: served to every viewer who can see the item.
func (f File) Teaser() bool { t, _ := f.Meta[MetaTeaser].(bool); return t }

// NewFailure records err for upload f: an ImageError keeps its code and details.
func NewFailure(f File, err error) *Failure {
	out := &Failure{Of: f.Key(), Message: err.Error()}
	if ie := AsImageError(err); ie != nil {
		out.Message, out.Code, out.Details = ie.Message, ie.Code, &ie.Details
	}
	return out
}

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
func (m *Manifest) Get(path string) (File, bool) {
	if i := m.Find(path); i >= 0 {
		return m.Files[i], true
	}
	return File{}, false
}

func (m *Manifest) reindex() {
	m.index = make(map[string]int, len(m.Files))
	for i, f := range m.Files {
		m.index[f.Path] = i
	}
}

// Outputs lists the derived files of preset from the upload (or zip
// prefix) from, in manifest order.
func (m *Manifest) Outputs(from, preset string) []File {
	var out []File
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
	seen := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		switch {
		case f.Path == "" || seen[f.Path]:
			return fmt.Errorf("media: manifest file %d: empty or duplicate path %q", i, f.Path)
		case f.Staged != "" && (!layout.ValidStagedName(f.Staged) || f.Blob != "" || f.Frame != nil || !f.IsUpload()):
			return fmt.Errorf("media: manifest file %q: invalid staged upload %q", f.Path, f.Staged)
		case f.Blob != "" && !layout.ValidHashName(f.Blob), f.Blob == "" && f.Staged == "" && (f.Frame == nil || !f.IsUpload()):
			return fmt.Errorf("media: manifest file %q: invalid blob %q", f.Path, f.Blob)
		case f.Track != nil && f.Track.Index != "" && !layout.ValidHashName(f.Track.Index):
			return fmt.Errorf("media: manifest file %q: invalid track index %q", f.Path, f.Track.Index)
		case !f.IsUpload() && (f.Edit != nil || f.Frame != nil || f.Pending != nil || f.Unattached):
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
	return nil
}

// Clone deep-copies the manifest for an edit: a cached manifest is shared,
// so an edit never mutates it.
func (m *Manifest) Clone() *Manifest {
	out := *m
	out.Meta = cloneMap(m.Meta)
	out.Files = make([]File, len(m.Files))
	for i, f := range m.Files {
		f.Meta, f.Pending = cloneMap(f.Meta), slices.Clone(f.Pending)
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
// upload's Pending. Outputs at paths taken by other files are an error.
func (m *Manifest) SetOutputs(from, preset string, outs []File) error {
	m.Files = slices.DeleteFunc(m.Files, func(f File) bool { return f.Preset == preset && f.From == from })
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

// encodeManifest writes m as gzip JSON.
func encodeManifest(m *Manifest) ([]byte, error) {
	m.V = ManifestVersion
	if m.Files == nil {
		m.Files = []File{}
	}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	if err := json.NewEncoder(zw).Encode(m); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// DecodeManifest reads a stored manifest (gzip JSON, or plain JSON).
func DecodeManifest(b []byte) (*Manifest, error) { return decodeManifest(b) }

// decodeManifest reads gzip JSON (or plain JSON) and indexes it.
func decodeManifest(b []byte) (*Manifest, error) {
	r := io.Reader(bytes.NewReader(b))
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = io.LimitReader(zr, maxManifestBytes+1)
	}
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, err
	}
	if m.V != ManifestVersion {
		return nil, fmt.Errorf("media: manifest version %d, want %d", m.V, ManifestVersion)
	}
	m.reindex()
	return &m, nil
}

// maxManifestBytes bounds a decoded manifest (a 2,000-page gallery is
// about 1.5 MB).
const maxManifestBytes = 64 << 20
