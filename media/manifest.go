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
// variants, HLS and downloads in blobs/.
type File struct {
	Name     string             `json:"name"`
	Original string             `json:"original"`
	Master   string             `json:"master,omitempty"`
	Type     string             `json:"type,omitempty"`
	Size     int64              `json:"size,omitempty"`
	Meta     map[string]any     `json:"meta,omitempty"`
	Variants map[string]Variant `json:"variants,omitempty"`
	HLS      *HLS               `json:"hls,omitempty"`
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
	Blob string `json:"blob"`
	Spec string `json:"spec,omitempty"`
	Type string `json:"type,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type Download struct {
	Blob   string `json:"blob"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Spec   string `json:"spec,omitempty"`
	Inputs string `json:"inputs,omitempty"` // hash of the ordered input blobs
}

// HLS is a byte-range ladder: each rendition is one fMP4 blob.
type HLS struct {
	Source string       `json:"source"`
	Spec   string       `json:"spec,omitempty"`
	Video  []Rendition  `json:"video,omitempty"`
	Audio  []AudioTrack `json:"audio,omitempty"`
	Subs   []Subtitle   `json:"subs,omitempty"`
	Sprite *Sprite      `json:"sprite,omitempty"`
}

type Rendition struct {
	Height    int       `json:"height"`
	Width     int       `json:"w"`
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
			fn(AreaBlobs, v.Blob)
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
	}
	var err error
	m.walk(func(area, name string) {
		if err == nil && !layout.ValidBlobName(name) {
			err = fmt.Errorf("media: manifest: invalid %s reference %q", area, name)
		}
	})
	return err
}
