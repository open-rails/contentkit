package media

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
)

// ManifestVersion is the manifest format.
const ManifestVersion = 3

// PublicationLayoutVersion marks explicit separation of retained sources,
// prepared files and served files in a version-owned item.
const PublicationLayoutVersion = 1

// CommitReceipt proves which prepared attempt reached S3. Every writer keeps
// the receipt until recovery has settled it; it is not an authorization token.
type CommitReceipt struct {
	Operation string `json:"operation"`
	Attempt   string `json:"attempt"`
}

// File is one file: an upload (no Preset) or a derived file.
type File struct {
	Path string  `json:"path"`           // app path: "originals/001.png", "low-res/001.webp"
	Blob string  `json:"blob,omitempty"` // "sha256-{hex}-{uuid}" in private/; "" for a staged upload or a frame not grabbed yet
	Type string  `json:"type"`
	Size int64   `json:"size,omitempty"`
	W    int     `json:"w,omitempty"` // an upload's oriented size once measured; an output's size
	H    int     `json:"h,omitempty"`
	Dur  float64 `json:"dur,omitempty"`

	// Uploads:
	Admission  *SourceAdmission `json:"admission,omitempty"`  // verified ZIP source quota policy, never client-supplied
	Staged     string           `json:"staged,omitempty"`     // "u-{uuid}" in temp/ until the worker hashes and places it at Blob
	CreateID   string           `json:"create_id,omitempty"`  // create-only put receipt; retained through placement and processing
	Edit       *Edit            `json:"edit,omitempty"`       // crop and rotate in source pixels
	Frame      *Frame           `json:"frame,omitempty"`      // grabbed from a video (the frame op)
	Meta       map[string]any   `json:"meta,omitempty"`       // lang, label, …
	Unattached bool             `json:"unattached,omitempty"` // processed on upload, not yet part of the item
	Gone       bool             `json:"gone,omitempty"`       // blob dropped (KeepOriginals false); the hash stays for provenance
	Pending    []string         `json:"pending,omitempty"`    // presets still producing from this upload
	Failed     *Failure         `json:"failed,omitempty"`     // this blob and edit cannot be processed
	Public     []Publication    `json:"public,omitempty"`     // reserved or published public generations
	Editor     *EditorImage     `json:"editor,omitempty"`     // selected immutable editor view, never listed to ordinary readers

	// Derived files:
	From     string `json:"from,omitempty"`     // the upload's path, or a zip's prefix
	Preset   string `json:"preset,omitempty"`   // the Private preset
	FP       string `json:"fp,omitempty"`       // Fingerprint of its inputs
	Download string `json:"download,omitempty"` // the human download name
	Track    *Track `json:"track,omitempty"`    // HLS
}

// EditorImage records the rendered view's allocation and source/spec fingerprint.
type EditorImage struct {
	Blob string `json:"blob"`
	FP   string `json:"fp"`
}

// Publication owns one public preset's physical files. Reservation precedes
// every PUT; State becomes ready only after all renditions have landed. Retired
// generations are never reused, even when their source bytes are identical.
// An upload holds at most one ready publication per preset, the current one,
// and one reserved successor, which replaces it once ready: cleanup never
// retires the current files while their successor is written.
type Publication struct {
	Preset string `json:"preset"`
	Source string `json:"source"` // the upload's Key
	FP     string `json:"fp"`
	// Stable records whether this generation also owns its logical names.
	// Nil marks historical manifests whose alias policy was not recorded.
	Stable *bool `json:"stable,omitempty"`
	// Generation suffixes every physical name; "" is a legacy publication,
	// files a pre-v0.68 worker wrote at the logical names (always ready).
	Generation string           `json:"generation"`
	Blobs      []string         `json:"blobs,omitempty"` // version preset outputs prepared in internal/, aligned with Names
	Names      []string         `json:"names"`           // logical preset names at this position
	State      PublicationState `json:"state"`
	Dims       []Dims           `json:"dims"`
}

// PublicationState has a fixed-width JSON representation: reserving and
// publishing a replacement must not consume Full manifests' flag headroom.
type PublicationState uint8

const (
	PublicationReserved PublicationState = iota
	PublicationReady
)

func (p Publication) Ready() bool { return p.State == PublicationReady }

// NamesOnDisk lists the immutable physical names reserved by the publication.
func (p Publication) NamesOnDisk() []string {
	if p.Generation == "" {
		return slices.Clone(p.Names)
	}
	names := make([]string, len(p.Names))
	for i, name := range p.Names {
		ext := path.Ext(name)
		names[i] = strings.TrimSuffix(name, ext) + "-" + p.Generation + ext
	}
	return names
}

// Publication returns the upload's latest generation of preset: its reserved
// successor if any, else its current one.
func (f File) Publication(preset string) (Publication, bool) {
	if p, ok := f.publication(preset, PublicationReserved); ok {
		return p, true
	}
	return f.Current(preset)
}

// Current returns the upload's ready generation of preset.
func (f File) Current(preset string) (Publication, bool) {
	return f.publication(preset, PublicationReady)
}

func (f File) publication(preset string, state PublicationState) (Publication, bool) {
	for _, p := range f.Public {
		if p.Preset == preset && p.State == state {
			return p, true
		}
	}
	return Publication{}, false
}

// Frame is an upload grabbed from a frame of its Upload.Frames video: the
// one a player shows T seconds in; Auto lets the worker choose T. Of is the video blob it was
// grabbed from: a new video grabs again. From names another item's video
// instead (Kind always set): grabbed once, it is a still that video's
// changes leave alone.
type Frame struct {
	T    float64   `json:"t,omitempty"`
	Auto bool      `json:"auto,omitempty"`
	Of   string    `json:"of,omitempty"`
	From *CopyFrom `json:"from,omitempty"`
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
	Index     string `json:"index,omitempty"` // "sha256-{hex}-{uuid}": the TrackIndex
}

const (
	TrackVideo = "video"
)

const (
	TrackAudio = "audio"
)

const (
	TrackSubs = "subs"
)

const (
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

const (
	MaxManifestBytes = 8 << 20
)

const (
	MaxMetaBytes = 4 << 10 // an upload's meta, as JSON
)

const (
	MaxItemMetaBytes = 16 << 10 // the item's meta, as JSON
)

const (
	MaxUploads = 10000
)
