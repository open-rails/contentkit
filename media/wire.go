package media

import (
	"net/http"
	"time"
)

// The HTTP wire types of the upload API (UploadHandler) and the read API
// (Reader.Handler). The browser SDK's types are generated from them
// (media/internal/wirets). SHA-256 values are lowercase hex.

// RefBody names an item of the handler's registry.
type RefBody struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// PresignBody declares one upload: its path, type, size and whole-file
// SHA-256 (the browser hashes every file, so every blob lands at its
// content address).
type PresignBody struct {
	Ref    RefBody `json:"ref"`
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Size   int64   `json:"size"`
	SHA256 string  `json:"sha256"`
}

// PresignReply is the upload plan: Exists (commit directly), one Put, or a
// Multipart upload. Path is the path to commit: cleaned, with an
// extension, and named by the server for a Named upload.
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

type CompleteReply struct {
	Blob string `json:"blob"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// CommitBody applies ops to an item in one conditional write.
type CommitBody struct {
	Ref RefBody `json:"ref"`
	Ops []Op    `json:"ops"`
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

// Access levels in ReadResult.
const (
	AccessFull    = "full"    // every file
	AccessPreview = "preview" // the first preview_limit pages' files, plus teasers
	AccessNone    = "none"    // teasers only
)

// ReadResult is the read API's answer: the files under the requested prefix
// in manifest order (Total of them), each with a signed URL within
// [offset, offset+limit) when the viewer may have it, else locked.
type ReadResult struct {
	Access       string         `json:"access"`
	PreviewLimit int            `json:"preview_limit"`
	Expires      int64          `json:"expires"` // unix seconds; URLs and the cookie stop working then
	Meta         map[string]any `json:"meta,omitempty"`
	Total        int            `json:"total"`
	Offset       int            `json:"offset"`
	Limit        int            `json:"limit"`
	// HLS lists the item's playable ladders by output directory ("hls/"):
	// play {read API}/{kind}/{id}/hls/{dir}master.m3u8.
	HLS []string `json:"hls,omitempty"`
	// State is the item's readiness (editors).
	State string     `json:"state,omitempty"`
	Files []FileInfo `json:"files"`
	// Cookie must be set on the response (cookie delivery, full access).
	Cookie *http.Cookie `json:"-"`
}

// FileInfo is one file of a read.
type FileInfo struct {
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Size     int64   `json:"size,omitempty"`
	W        int     `json:"w,omitempty"`
	H        int     `json:"h,omitempty"`
	Dur      float64 `json:"dur,omitempty"`
	Teaser   bool    `json:"teaser,omitempty"`
	Download string  `json:"download,omitempty"` // the name a download read signs
	URL      string  `json:"url,omitempty"`
	Locked   bool    `json:"locked,omitempty"`

	// Editors (editor reads and commit replies):
	Upload     bool            `json:"upload,omitempty"`
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
