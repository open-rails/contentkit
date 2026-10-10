package media

import (
	"net/http"
	"time"
)

// The HTTP wire types of the upload API (UploadHandler) and the read API
// (Reader.Handler). The browser SDK's types are generated from them
// (internal/contract). SHA-256 values are lowercase hex.

// RefBody names an item of the handler's registry.
type RefBody struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// PresignBody declares one upload: its path, type, size and whole-file
// SHA-256, which finds an identical blob already in the folder and binds a
// single PUT's body.
type PresignBody struct {
	Ref    RefBody `json:"ref"`
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Size   int64   `json:"size"`
	SHA256 string  `json:"sha256"`
}

// PresignReply is the upload plan: Exists (commit directly), one Put, or a
// Multipart upload. Path is the path to commit: cleaned, with an
// extension, and named by the server for a Named upload. Blob is the name
// to commit: the folder's blob (sha256-{hex}-{uuid}) with Exists, else the staged
// upload (u-{uuid}) the PUT or parts write, which the worker places.
type PresignReply struct {
	Path            string          `json:"path"`
	Blob            string          `json:"blob"`
	Exists          bool            `json:"exists,omitempty"`
	Put             *RequestReply   `json:"put,omitempty"`
	Multipart       *MultipartReply `json:"multipart,omitempty"`
	ProcessOnUpload bool            `json:"process_on_upload,omitempty"`
}

// RequestReply is a presigned request: send exactly these headers (the
// browser adds Content-Length, which is signed too).
type RequestReply struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

type MultipartReply struct {
	Ticket      string `json:"ticket"`
	MinPartSize int64  `json:"min_part_size"`
	MaxPartSize int64  `json:"max_part_size"`
	MaxParts    int    `json:"max_parts"`
}

type PartBody struct {
	Number int32  `json:"number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type PartsBody struct {
	Ticket string     `json:"ticket"`
	Parts  []PartBody `json:"parts"`
}

type TicketBody struct {
	Ticket string `json:"ticket"`
}

// PartReply is a presigned part (parts) or a landed part (parts/list).
type PartReply struct {
	Number  int32         `json:"number"`
	Size    int64         `json:"size,omitempty"`
	SHA256  string        `json:"sha256,omitempty"`
	Request *RequestReply `json:"request,omitempty"`
}

type PartsReply struct {
	Parts []PartReply `json:"parts"`
}

// CompleteReply is a completed multipart upload; Blob is its staged name.
type CompleteReply struct {
	Blob string `json:"blob"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// CommitBody applies ops to an item in one conditional write.
type CommitBody struct {
	Ref         RefBody `json:"ref"`
	OperationID string  `json:"operation_id"` // stable UUID for this complete ordered batch
	Ops         []Op    `json:"ops"`
}

// CommitReply is the item's uploads as an editor reads them.
type CommitReply struct {
	Files []FileInfo `json:"files"`
}

// ErrorReply is the error body; Code is one of the Code* constants,
// "unauthorized" or "internal_error".
type ErrorReply struct {
	Error      string        `json:"error"`
	Code       string        `json:"code"`
	RetryAfter int           `json:"retry_after,omitempty"` // seconds, with 429
	Blobs      []string      `json:"blobs,omitempty"`       // not_uploaded at commit: the blobs to upload again
	Details    *ErrorDetails `json:"details,omitempty"`     // image refusals
}

// Access levels in ReadResult: an item's private files are all or nothing.
const (
	AccessFull = "full" // every private file
	AccessNone = "none" // public files only (previews)
)

// ReadResult is the read API's answer: the files under the requested prefix
// in manifest order (Total of them). With access each has a URL within
// [offset, offset+limit); without, each is locked: its path, type and size.
type ReadResult struct {
	Access string `json:"access"`
	// Expires is when the item token (the URLs, the cookie) stops working,
	// in unix seconds. A read before then answers the same token, so a
	// client keeps what it has and reads again shortly before.
	Expires int64          `json:"expires"`
	Meta    map[string]any `json:"meta,omitempty"`
	// Previews are the item's public preview images in order (a Public
	// preset with First): every viewer who can see the item gets them.
	Previews []string `json:"previews,omitempty"`
	// Public lists the exact published renditions. Clients must not infer a
	// current cover or avatar's physical filename from the preset template.
	Public []PublicImage `json:"public,omitempty"`
	Total  int           `json:"total"`
	Offset int           `json:"offset"`
	Limit  int           `json:"limit"`
	// HLS lists the item's playable ladders by output directory ("hls/"):
	// play {read API}/{kind}/{id}/hls/{dir}master.m3u8.
	HLS []string `json:"hls,omitempty"`
	// State is the item's readiness (editors).
	State string `json:"state,omitempty"`
	// Full (editors): the item's manifest cannot hold more outputs, so
	// processing stopped; remove uploads (any commit that shrinks it) to
	// resume. Uploads left unprocessed stay pending.
	Full bool `json:"full,omitempty"`
	// Uploads (editors) are the kind's upload paths and their rules.
	Uploads []UploadRule `json:"uploads,omitempty"`
	Files   []FileInfo   `json:"files"`
	// Cookie must be set on the response (cookie delivery, with access).
	Cookie *http.Cookie `json:"-"`
}

// PublicImage is a published public preset for one upload.
type PublicImage struct {
	From       string            `json:"from"`
	Preset     string            `json:"preset"`
	Renditions []PublicRendition `json:"renditions"`
}

// PublicRendition carries its physical URL and actual encoded dimensions.
type PublicRendition struct {
	URL string `json:"url"`
	W   int    `json:"w"`
	H   int    `json:"h"`
}

// FileInfo is one file of a read.
type FileInfo struct {
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Size     int64   `json:"size,omitempty"`
	W        int     `json:"w,omitempty"`
	H        int     `json:"h,omitempty"`
	Dur      float64 `json:"dur,omitempty"`
	Download string  `json:"download,omitempty"` // the name a download read serves it under
	URL      string  `json:"url,omitempty"`
	Locked   bool    `json:"locked,omitempty"`

	// Editors (editor reads and commit replies):
	Upload     bool            `json:"upload,omitempty"`
	Staged     bool            `json:"staged,omitempty"` // uploaded, not yet placed by the worker: no blob, views or frames yet
	From       string          `json:"from,omitempty"`
	Edit       *Edit           `json:"edit,omitempty"`
	Frame      *Frame          `json:"frame,omitempty"`
	Meta       map[string]any  `json:"meta,omitempty"`
	Unattached bool            `json:"unattached,omitempty"`
	Pending    []string        `json:"pending,omitempty"`
	Failed     *Failure        `json:"failed,omitempty"`
	EditorURL  string          `json:"editor_url,omitempty"`
	Progress   *EncodeProgress `json:"progress,omitempty"`
}

// UploadRule is one upload path's rules, as editor reads carry them so a
// client turns away a file the server would refuse. The server stays the
// authority.
type UploadRule struct {
	Path     string   `json:"path"`
	Types    []string `json:"types"`
	MaxBytes int64    `json:"max_bytes"`
	// Max caps the files at Path; 0 is unlimited.
	Max    int    `json:"max,omitempty"`
	Named  bool   `json:"named,omitempty"`
	Frames string `json:"frames,omitempty"`
	// Aspect and MinWidth bound an image's edit: its first public preset's.
	Aspect   Aspect `json:"aspect,omitzero"`
	MinWidth int    `json:"min_width,omitempty"`
	// Video is a video or audio upload's limits in effect; MinAspect and
	// MaxAspect the display aspects (width/height) its HLS presets accept.
	Video     *VideoLimits `json:"video,omitempty"`
	MinAspect float64      `json:"min_aspect,omitempty"`
	MaxAspect float64      `json:"max_aspect,omitempty"`
}

// PresetRule is a kind's public preset: its upload, name template, widths
// and crop bounds. Published files carry a generation suffix, so
// {base}/v1/{namespace}/{kind}/{id}/public/{to} ({w} each of Widths) names
// the kind's default image, never an item's current one: a read's Public
// lists those.
type PresetRule struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	From      string `json:"from"`
	Base      string `json:"base"`
	Namespace string `json:"namespace"`
	To        string `json:"to"`
	Widths    []int  `json:"widths"`
	// Aspect is the rendition's shape ("W:H"); absent keeps the edit's.
	Aspect Aspect `json:"aspect,omitzero"`
	// MinWidth is the narrowest edit the preset accepts.
	MinWidth int `json:"min_width,omitempty"`
	// First makes it a preview: the first First uploads, {n} in To.
	First int `json:"first,omitempty"`
}
