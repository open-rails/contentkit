package content

import (
	"encoding/json"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

const (
	ModerationApproved = "approved"
)

const (
	ModerationHeld = "held"
)

const (
	ModerationRejected = "rejected"
)

// verdictMeta is the moderation_verdict jsonb: screening provenance kept for
// the reviewer. Error records a moderator failure that forced the hold.
type verdictMeta struct {
	Model         string  `json:"model,omitempty"`
	PromptVersion string  `json:"prompt_version,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// HeldItem is one comment awaiting review.
type HeldItem struct {
	Kind     string `json:"kind"`     // KindComment
	Revision int64  `json:"revision"` // required when resolving this exact screened text
	ID       string `json:"id"`
	// Ref is the commented content, or the held item's own reference.
	Ref           contentref.ContentRef `json:"ref"`
	AuthorID      string                `json:"author_id,omitempty"`
	Author        *PublicUser           `json:"author,omitempty"`
	AnonName      string                `json:"anon_name,omitempty"`
	Title         string                `json:"title,omitempty"`
	Body          string                `json:"body"`
	Reason        string                `json:"reason"`
	Model         string                `json:"model,omitempty"`
	PromptVersion string                `json:"prompt_version,omitempty"`
	Confidence    float64               `json:"confidence,omitempty"`
	Error         string                `json:"error,omitempty"` // moderator failure that forced the hold
	HeldAt        time.Time             `json:"held_at"`
	CreatedAt     time.Time             `json:"created_at"`
}

// HeldPage is one page of the review queue; Next resumes after the last item.
type HeldPage struct {
	Items []HeldItem `json:"items"`
	Next  string     `json:"next,omitempty"`
}

// ReviewDecision resolves a held item: approve publishes it, reject keeps it
// author-only with Reason (or the moderator's reason when empty).
type ReviewDecision struct {
	Revision int64 // the revision returned by ListHeld; stale decisions cannot publish edits
	Decision Decision
	Reviewer string
	Reason   string
}

// SetVerdict fills the screening provenance from a moderation_verdict.
func (it *HeldItem) SetVerdict(meta []byte) {
	var m verdictMeta
	if len(meta) > 0 && json.Unmarshal(meta, &m) == nil {
		it.Model, it.PromptVersion, it.Confidence, it.Error = m.Model, m.PromptVersion, m.Confidence, m.Error
	}
}

// ResolveInput resolves a held item at the revision the queue listed; Reason
// replaces the moderator's on a rejection.
type ResolveInput struct {
	Decision Decision `json:"decision"`
	Revision int64    `json:"revision"`
	Reason   string   `json:"reason,omitempty"`
}

// ReviewOutcome is the decision applied.
type ReviewOutcome struct {
	Decision Decision `json:"decision"`
}
