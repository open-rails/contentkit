// Package access is the host's gating vocabulary shared by content
// interactions and media: the authenticated Actor, the ContentResolver port
// and its Resolution. It has no dependencies beyond contentref.
package access

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
)

// Actor is the already-authenticated caller. ContentKit never authenticates.
type Actor struct {
	ID        string // stable subject id (uuid text); empty when Anonymous
	Kind      string // opaque: "user" | "service" | "delegated" | ...
	IP        string // anon fallback key for reactions / poll votes
	Anonymous bool
}

// Resolution is the host's verdict about a content reference for one actor.
type Resolution struct {
	// Ref is the canonical reference every row is stored and read under (an
	// alias or slug resolves to it). A zero Ref keeps the requested one; a Ref
	// of another tenant is an error.
	Ref contentref.ContentRef
	// Visible = published and not soft-deleted. Teasers need only Visible.
	Visible bool
	// Accessible = the actor may consume it: an opaque host verdict
	// (entitlement, purchase, ACL, flag). ContentKit imposes no access model.
	Accessible bool
	// PreviewLimit caps a Visible item to its first N ordered units (pages,
	// files), whatever Accessible says. 0 = no cap. Content interactions
	// ignore it.
	PreviewLimit int
}

// Full reports unrestricted access: every unit is served.
func (r Resolution) Full() bool {
	return r.Visible && r.Accessible && r.PreviewLimit <= 0
}

// Units returns how many leading units of an ordered list of total units the
// resolution grants.
func (r Resolution) Units(total int) int {
	switch {
	case !r.Visible || total <= 0:
		return 0
	case r.PreviewLimit > 0:
		return min(r.PreviewLimit, total)
	case r.Accessible:
		return total
	}
	return 0
}

// ContentResolver is the one mandatory content hook and the whole gating
// surface: it says whether a ContentRef exists, is visible and is accessible
// to the actor. Callers invoke it once per item per request; an error denies.
type ContentResolver interface {
	Resolve(ctx context.Context, ref contentref.ContentRef, actor Actor) (Resolution, error)
}
