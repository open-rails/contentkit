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
	PostWrite       string // create/update/delete posts
	PollWrite       string // create/update/delete polls + options
	CommentModerate string // moderator delete/restore of another actor's comment
}
