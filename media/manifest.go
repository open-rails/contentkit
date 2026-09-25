package media

import (
	"encoding/json"
	"fmt"

	"github.com/open-rails/contentkit/media/layout"
)

// Manifest is the ordered file list of an item or version. List order is
// display order. Blob references are names within the item's folder.
type Manifest struct {
	Files     []File              `json:"files"`
	Meta      map[string]any      `json:"meta,omitempty"`
	Downloads map[string]Download `json:"downloads,omitempty"`
}

// File is one manifest entry. Original and Master live in originals/;
// variants, HLS and downloads in blobs/, EditorOnly variants in editor/. Image variants derive from Source()
// through Edit; Dims is Source()'s size, recorded by processing, and edits
// are validated against it. meta w/h is the edited size.
type File struct {
	Name     string             `json:"name"`
	Original string             `json:"original"`
	Master   string             `json:"master,omitempty"`
	Type     string             `json:"type,omitempty"`
	Size     int64              `json:"size,omitempty"`
	Edit     *Edit              `json:"edit,omitempty"`
	Dims     *Dims              `json:"dims,omitempty"`
	Meta     map[string]any     `json:"meta,omitempty"`
	Variants map[string]Variant `json:"variants,omitempty"`
	HLS      *HLS               `json:"hls,omitempty"`
	// Failure is why the image processor cannot derive this source through
	// this edit (Of); a new source or edit clears it.
	Failure *FileFailure `json:"failure,omitempty"`
}

// FileFailure is a file's permanent processing failure.
type FileFailure struct {
	Of      string        `json:"of"`             // File.FailureKey it was recorded for
	Message string        `json:"message"`        // what went wrong, or the rule an ImageError states
	Code    string        `json:"code,omitempty"` // an ImageError's code
	Details *ErrorDetails `json:"details,omitempty"`
}

// FailureKey identifies the source and edit a Failure applies to.
func (f File) FailureKey() string { return f.Source() + "." + f.Edit.Hash() }

// Failed is the file's failure for its current source and edit, or nil.
func (f File) Failed() *FileFailure {
	if f.Failure != nil && f.Failure.Of == f.FailureKey() {
		return f.Failure
	}
	return nil
}

// NewFileFailure records err for f: an ImageError keeps its code and details.
func NewFileFailure(f File, err error) *FileFailure {
	out := &FileFailure{Of: f.FailureKey(), Message: err.Error()}
	if ie := AsImageError(err); ie != nil {
		out.Message, out.Code, out.Details = ie.Message, ie.Code, &ie.Details
	}
	return out
}

// Source is the file variants derive from: Master when present, else Original.
func (f File) Source() string {
	if f.Master != "" {
		return f.Master
	}
	return f.Original
}

// Teaser reports meta.teaser, a file served to any viewer who can see the item.
func (f File) Teaser() bool { t, _ := f.Meta["teaser"].(bool); return t }

type Variant struct {
	Blob   string `json:"blob"`
	Spec   string `json:"spec,omitempty"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Editor bool   `json:"editor,omitempty"` // from Spec.EditorOnly: in editor/, signed for editors only
}

type Download struct {
	Blob   string `json:"blob"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Spec   string `json:"spec,omitempty"`
	Inputs string `json:"inputs,omitempty"` // hash of the ordered input blobs
}

// HLS is a byte-range ladder: each rendition is one fMP4 blob. Source is the
// original it was encoded from; when it differs from the file's, the ladder is
// stale but still served until its replacement is promoted. Error, with no
// renditions, records why Source can never be encoded.
type HLS struct {
	Source string       `json:"source"`
	Spec   string       `json:"spec,omitempty"`
	Error  string       `json:"error,omitempty"`
	Video  []Rendition  `json:"video,omitempty"`
	Audio  []AudioTrack `json:"audio,omitempty"`
	Subs   []Subtitle   `json:"subs,omitempty"`
	Sprite *Sprite      `json:"sprite,omitempty"`
	// Pending lists the rungs of a later encode stage: the file plays at the
	// rungs in Video until they are added.
	Pending []int `json:"pending,omitempty"`
}

// Rendition is one video-only fMP4 blob: its init segment is bytes
// [0, Segments[0].Offset) and the segments follow contiguously. Rung is its
// ladder label (the short side it was asked for, e.g. 1080 for "1080p");
// Width and Height are the encoded frame.
type Rendition struct {
	Rung      int       `json:"rung"`
	Width     int       `json:"w"`
	Height    int       `json:"h"`
	Bandwidth int       `json:"bandwidth"`
	Average   int       `json:"avg,omitempty"`
	Codecs    string    `json:"codecs"`
	Blob      string    `json:"blob"`
	Segments  []Segment `json:"segments"`
}

type AudioTrack struct {
	ID        string    `json:"id"`
	Lang      string    `json:"lang,omitempty"`
	Label     string    `json:"label,omitempty"`
	Default   bool      `json:"default,omitempty"`
	Bandwidth int       `json:"bandwidth,omitempty"`
	Codecs    string    `json:"codecs,omitempty"`
	Blob      string    `json:"blob"`
	Segments  []Segment `json:"segments"`
}

type Subtitle struct {
	ID     string `json:"id"`
	Lang   string `json:"lang,omitempty"`
	Label  string `json:"label,omitempty"`
	Forced bool   `json:"forced,omitempty"`
	Blob   string `json:"blob"`
}

type Sprite struct {
	Blob     string  `json:"blob"`
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	Width    int     `json:"w"`
	Height   int     `json:"h"`
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

// File returns the index of the named file, or -1.
func (m *Manifest) File(name string) int {
	for i := range m.Files {
		if m.Files[i].Name == name {
			return i
		}
	}
	return -1
}

// Blobs returns every blobs/ name the manifest references.
func (m *Manifest) Blobs() []string { return m.names(AreaBlobs) }

// EditorBlobs returns every editor/ name the manifest references (EditorOnly variants).
func (m *Manifest) EditorBlobs() []string { return m.names(AreaEditor) }

// Originals returns every originals/ name the manifest references.
func (m *Manifest) Originals() []string { return m.names(AreaOriginals) }

func (m *Manifest) names(area string) []string {
	var out []string
	m.walk(func(a, name string) {
		if a == area && name != "" {
			out = append(out, name)
		}
	})
	return out
}

// walk visits every required reference; an unset one is visited as "".
func (m *Manifest) walk(fn func(area, name string)) {
	for _, f := range m.Files {
		fn(AreaOriginals, f.Original)
		if f.Master != "" {
			fn(AreaOriginals, f.Master)
		}
		for _, v := range f.Variants {
			if v.Editor {
				fn(AreaEditor, v.Blob)
			} else {
				fn(AreaBlobs, v.Blob)
			}
		}
		if h := f.HLS; h != nil {
			fn(AreaOriginals, h.Source)
			for _, r := range h.Video {
				fn(AreaBlobs, r.Blob)
			}
			for _, a := range h.Audio {
				fn(AreaBlobs, a.Blob)
			}
			for _, s := range h.Subs {
				fn(AreaBlobs, s.Blob)
			}
			if h.Sprite != nil {
				fn(AreaBlobs, h.Sprite.Blob)
			}
		}
	}
	for _, d := range m.Downloads {
		fn(AreaBlobs, d.Blob)
	}
}

// Validate requires unique, non-empty file names and well-formed references.
func (m *Manifest) Validate() error {
	seen := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		if f.Name == "" || seen[f.Name] {
			return fmt.Errorf("media: manifest file %d: empty or duplicate name %q", i, f.Name)
		}
		seen[f.Name] = true
		var w, h int
		if f.Dims != nil {
			w, h = f.Dims.W, f.Dims.H
		}
		if err := f.Edit.Check(w, h); err != nil {
			return fmt.Errorf("media: manifest file %q: edit: %w", f.Name, err)
		}
	}
	var err error
	m.walk(func(area, name string) {
		if err == nil && !layout.ValidBlobName(name) {
			err = fmt.Errorf("media: manifest: invalid %s reference %q", area, name)
		}
	})
	return err
}
