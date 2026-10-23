// Package content is ContentKit's interaction module: comments, reactions,
// favorites and polls over tenant-scoped content references, stored
// in the host schema's content_* interaction tables. Everything host-specific
// lives behind the ports in this file; the package imports no sibling kit and
// bakes in no host assumption. Content kinds are host-registered, access is an opaque host
// verdict, ids are opaque text, and every key, index and cursor carries the
// tenant pinned at construction.
package content

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// PublicUser is how an author is shown: its name and username from the
// host's directory (Options.UserInfo) and its avatar (Options.Avatars).
// Never its email.
type PublicUser struct {
	ID           string `json:"id"`
	Deleted      bool   `json:"deleted,omitempty"`
	Name         string `json:"name,omitempty"`
	Username     string `json:"username"`
	Avatar       string `json:"avatar,omitempty"`
	AvatarSrcSet string `json:"avatar_srcset,omitempty"`
}

// --- optional ports (nil -> documented default) ---

// ContentProcessor sanitizes rich text on write. Default: strip tags.
type ContentProcessor interface {
	Sanitize(ctx context.Context, raw string) (string, error)
}

// permissions are the staff permissions content checks (access.Permission).
var permissions = []access.Permission{access.PollsWrite, access.CommentsModerate, access.CommentsBan, access.ModerationReview}

// --- moderation and classification ports (nil -> publish / refuse) ---

// Decision is a ContentModerator's outcome for one text write.
type Decision string

const (
	DecisionApprove Decision = "approve" // publish
	DecisionReject  Decision = "reject"  // refuse the write: 422 with the reason, nothing stored
	DecisionReview  Decision = "review"  // store held: author-only until a reviewer resolves it
)

// ModerationInput is one comment, or a catalog item's text, about to publish.
// The moderator sees sanitized text and opaque ids only.
type ModerationInput struct {
	SubjectID string // opaque content author; may differ from editing moderator
	Tenant    string
	Actor     access.Actor
	// Ref is the content the item belongs to: the commented content for a
	// comment, the item's own reference for catalog text.
	Ref    contentref.ContentRef
	Kind   string // KindComment, or a HeldSource's kind (catalog.HeldKind)
	ItemID string // the existing item on an edit; empty on create
	Title  string // catalog text only
	Text   string
}

// Verdict is a moderator's decision with its provenance. Reason is shown to
// the author on reject and review; Model, PromptVersion and Confidence are
// kept with a held item for the reviewer.
type Verdict struct {
	Decision      Decision
	Reason        string
	Model         string
	PromptVersion string
	Confidence    float64
}

// ContentModerator screens every comment/post write before it publishes.
// Absent port: every write publishes. An error or an unknown decision fails
// closed to review: the submission is kept, held, never published unscreened.
type ContentModerator interface {
	Screen(ctx context.Context, in ModerationInput) (Verdict, error)
}

// Answer is one free-text poll answer handed to the AnswerClassifier.
type Answer struct {
	Tenant     string
	QuestionID string
	AnswerID   string
	Revision   int64  // monotonic per answer; retries retain this revision
	SubjectID  string // opaque authenticated actor, never an IP
	Text       string
}

// GroupAssignment is the group an answer was placed in at store time.
type GroupAssignment struct {
	GroupID string
	Label   string
}

// AnswerGroup is one answer group of a free-text poll with its current size.
// The ContentKit owns current assignments and counts, so delayed provider side
// effects cannot rewrite results.
type AnswerGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// AnswerClassifier groups free-text poll answers. Classify runs when an
// answer is stored or edited. Poll results come from source assignments. Without a
// registered classifier a free-text poll cannot be created.
// Classify must be idempotent by (Tenant, AnswerID, Revision), ignore older
// revisions. ContentKit accepts assignments only by source-revision CAS;
// the stored result is the sole authority for current membership and labels.
// A classifier retaining personal data needs Options.ProviderDataEraser.
type AnswerClassifier interface {
	Classify(ctx context.Context, a Answer) (GroupAssignment, error)
}

// StatelessPolicy explicitly declares that a policy implementation retains no
// personal data outside this process. Built-in deterministic policies satisfy
// this marker; retaining providers must instead configure ProviderDataEraser.
type StatelessPolicy interface{ StatelessPolicy() }
