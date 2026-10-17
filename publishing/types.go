// Package publishing accepts one archive and its catalog metadata as a durable,
// resumable publishing operation. Storage transfers still use the media module.
package publishing

import (
	"context"
	"encoding/json"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// Mode is the result requested after the archive has been processed.
type Mode string

const (
	Draft     Mode = "draft"
	WhenReady Mode = "when_ready"
)

type State string

const (
	AwaitingUpload State = "awaiting_upload"
	Processing     State = "processing"
	Publishing     State = "publishing"
	Scheduled      State = "scheduled"
	Ready          State = "ready"
	ReviewRequired State = "review_required"
	Published      State = "published"
	Blocked        State = "blocked"
	Failed         State = "failed"
)

// Archive declares the complete ZIP before any upload is authorized.
type Archive struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Source identifies an external record. URL is provenance, never a fetch URL.
type Source struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	URL      string `json:"url,omitempty"`
}

// EntityReference selects a group by ID or an exact localized name. Scope
// selects the parent kind required by the configured group kind.
type EntityReference struct {
	ID       string           `json:"id,omitempty"`
	Kind     string           `json:"kind,omitempty"`
	Name     string           `json:"name,omitempty"`
	Language string           `json:"language,omitempty"`
	Scope    *EntityReference `json:"scope,omitempty"`
}

// Request is immutable for an operation ID. One initial version is supported.
type Request struct {
	Content               catalog.ContentInput `json:"content"`
	Entities              []EntityReference    `json:"entities,omitempty"`
	Archive               Archive              `json:"archive"`
	Publish               Mode                 `json:"publish,omitempty"`
	PublishAt             *time.Time           `json:"publish_at,omitempty"`
	CreateMissingEntities bool                 `json:"create_missing_entities,omitempty"`
	Source                *Source              `json:"source,omitempty"`
}

// CompleteRequest accepts the operation's already-declared archive. No caller
// may substitute another blob, content ID, or metadata at completion time.
type CompleteRequest struct{}

type Progress struct {
	Stage     string `json:"stage"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

type OperationResult struct {
	Path  string `json:"path"`
	Pages int    `json:"pages"`
}

// OperationError describes a refused request or a stopped asynchronous operation.
type OperationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Field     string `json:"field,omitempty"`
}

func (f *OperationError) Error() string { return f.Message }

// Operation is the durable receipt. Upload appears only on Put, never Get.
type Operation struct {
	ID        string              `json:"id"`
	State     State               `json:"state"`
	ContentID string              `json:"content_id"`
	VersionID string              `json:"version_id"`
	PublishAt *time.Time          `json:"publish_at,omitempty"`
	Entities  []EntityResolution  `json:"entities"`
	Progress  Progress            `json:"progress"`
	Result    *OperationResult    `json:"result,omitempty"`
	Error     *OperationError     `json:"error,omitempty"`
	Upload    *media.PresignReply `json:"upload,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// Authority captures only provider-owned, nonsecret delegation data. Resume
// checks the original credential and its current permissions. A serialized
// auth.Verified, arbitrary identity, or bearer token is never a delegation.
// Nil means automatic publication is unavailable to external callers.
type Authority interface {
	Capture(context.Context, access.Actor) (json.RawMessage, error)
	Resume(context.Context, json.RawMessage) (access.Actor, error)
}

// EntityResolution identifies the canonical entity associated with this
// submission. Created remains true on every replay of the operation that
// created it. Scope reports an automatically resolved parent when needed.
type EntityResolution struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"`
	Name    string            `json:"name"`
	Created bool              `json:"created"`
	Scope   *EntityResolution `json:"scope,omitempty"`
}
