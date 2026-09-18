// Package content is ContentKit's interaction module: posts, comments,
// reactions, favorites and polls over tenant-scoped content references, stored
// in the host schema's social_* tables. Everything host-specific lives behind
// the ports in this file; the package imports no sibling kit and bakes in no
// host assumption. Content kinds are host-registered, access is an opaque host
// verdict, ids are opaque text, and every key, index and cursor carries the
// tenant pinned at construction.
package content

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
)

// Actor is the already-authenticated caller, read from context by the Identity
// port. ContentKit never authenticates.
type Actor struct {
	ID        string // stable subject id (uuid text); empty when Anonymous
	Kind      string // opaque: "user" | "service" | "delegated" | ...
	IP        string // anon fallback key for reactions / poll votes
	Anonymous bool
}

// PublicUser is display enrichment for an author/actor id.
type PublicUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar,omitempty"`
}

// Resolution is the host's verdict about a content reference.
type Resolution struct {
	// Ref is the canonical reference every row is stored and read under (an
	// alias or slug resolves to it). A zero Ref keeps the requested one; a Ref
	// of another tenant is an error.
	Ref contentref.ContentRef
	// Visible = published and not soft-deleted.
	Visible bool
	// Accessible = the actor may consume it: an opaque host verdict
	// (entitlement, purchase, ACL, flag). ContentKit imposes no access model.
	Accessible bool
}

// --- mandatory ports ---

// Identity reads the authenticated actor from context; the host's middleware
// populated it upstream.
type Identity interface {
	Actor(ctx context.Context) (Actor, bool)
}

// Authorizer answers whether an actor holds an opaque host permission. Callers
// are fail-closed: an error is never "allowed".
type Authorizer interface {
	Can(ctx context.Context, actor Actor, perm string) (bool, error)
}

// ContentResolver is the one mandatory content hook and the whole gating
// surface: it says whether a ContentRef exists, is visible and is accessible.
// Report absence either through the sentinel errors (ErrNotFound /
// ErrNotVisible / ErrForbidden) or through the Resolution flags.
type ContentResolver interface {
	Resolve(ctx context.Context, ref contentref.ContentRef, actor Actor) (Resolution, error)
}

// ContentCanonicalizer maps a resolved reference to the one reference an
// actor's reaction or favorite is recorded, counted and exported under:
// per-language routes ("42:en", "42:ja") collapse to one content_id; an
// explicit content_version_id stays a distinct key; a language suffix never
// implies a version. ok=false keeps the target out of the preference boundary
// (its rows keep the resolver's reference and nothing is exported). Comment
// threads never pass through it. Nil disables export: standalone hosts need no
// analytics sink.
type ContentCanonicalizer interface {
	Canonical(ref contentref.ContentRef) (contentref.ContentRef, bool)
}

// ContentCanonicalizerFunc adapts a function to the ContentCanonicalizer port.
type ContentCanonicalizerFunc func(ref contentref.ContentRef) (contentref.ContentRef, bool)

func (f ContentCanonicalizerFunc) Canonical(ref contentref.ContentRef) (contentref.ContentRef, bool) {
	return f(ref)
}

// UserEnricher batch-loads display data for author/actor ids.
type UserEnricher interface {
	UsersByIDs(ctx context.Context, ids []string) (map[string]PublicUser, error)
}

// --- optional ports (nil -> documented default) ---

// MediaStore stores option/cover images. Default: uploads are unsupported.
type MediaStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) (url string, err error)
}

// ContentProcessor sanitizes rich text on write. Default: strip tags.
type ContentProcessor interface {
	Sanitize(ctx context.Context, raw string) (string, error)
}

// Perms carries the opaque host permission strings checked through
// Authorizer.Can before privileged writes. An unset gate fails closed.
type Perms struct {
	PostWrite        string // create/update/delete posts
	PollWrite        string // create/update/delete polls + options
	CommentModerate  string // moderator delete/restore of another actor's comment
	ModerationReview string // list and resolve held comments and posts
}

// --- moderation and classification ports (nil -> publish / refuse) ---

// Decision is a ContentModerator's outcome for one text write.
type Decision string

const (
	DecisionApprove Decision = "approve" // publish
	DecisionReject  Decision = "reject"  // refuse the write: 422 with the reason, nothing stored
	DecisionReview  Decision = "review"  // store held: author-only until a reviewer resolves it
)

// ModerationInput is one comment or post body about to publish. The moderator
// sees sanitized text and opaque ids only.
type ModerationInput struct {
	Tenant string
	Actor  Actor
	// Ref is the content the item belongs to: the commented work for a
	// comment, the post's own reference for a post.
	Ref    contentref.ContentRef
	Kind   string // KindComment | KindPost
	ItemID string // the existing item on an edit; empty on create
	Title  string // posts only
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
	Text       string
}

// GroupAssignment is the group an answer was placed in at store time.
type GroupAssignment struct {
	GroupID string
	Label   string
}

// Group is one answer group of a free-text poll with its current size. The
// classifier owns the assignments (it may re-cluster); ContentKit only keeps
// which answers are still unclassified.
type Group struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// AnswerClassifier groups free-text poll answers. Classify runs when an
// answer is stored or edited; Groups is read with the poll results. Without a
// registered classifier a free-text poll cannot be created.
type AnswerClassifier interface {
	Classify(ctx context.Context, a Answer) (GroupAssignment, error)
	Groups(ctx context.Context, tenant, questionID string) ([]Group, error)
}
