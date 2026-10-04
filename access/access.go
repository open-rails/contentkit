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
	// alias or slug resolves to it). A zero Ref keeps the requested one, which
	// content then refuses unless lower case; a Ref of another tenant is an
	// error.
	Ref contentref.ContentRef
	// Visible = published and not soft-deleted. Media's public files (covers,
	// previews) need only Visible to anonymous viewers.
	Visible bool
	// Accessible = the actor may consume it: an opaque host verdict
	// (entitlement, purchase, ACL, flag). ContentKit imposes no access model.
	// For media it is all or nothing: every private file of the item, or none.
	Accessible bool
	// Editor = the actor may edit the item (its creator, staff): media
	// returns edit metadata and editor views only for editors, and gives a
	// visible item's editor its private files whatever Accessible says.
	Editor bool
	// Owner is the actor id that owns the content (its creator), "" when no
	// single user does. A comment ban in that owner's scope applies to it.
	Owner string
}

// Full reports access to the item: everything private is served.
func (r Resolution) Full() bool {
	return r.Visible && r.Accessible
}

// ContentResolver is the one mandatory content hook and the whole gating
// surface: it says whether each ContentRef exists, is visible and is
// accessible to the actor. It is batch-first: callers pass every ref a
// request needs in one call (a single item is a batch of one). The map is
// keyed by each requested ref's Key; a ref missing from it denies. An error
// fails the whole batch and denies every ref.
type ContentResolver interface {
	Resolve(ctx context.Context, refs []contentref.ContentRef, actor Actor) (map[contentref.ContentKey]Resolution, error)
}

// ResolveOne resolves a single ref as a batch of one; an omitted ref yields
// the zero (denying) Resolution.
func ResolveOne(ctx context.Context, r ContentResolver, ref contentref.ContentRef, actor Actor) (Resolution, error) {
	m, err := r.Resolve(ctx, []contentref.ContentRef{ref}, actor)
	if err != nil {
		return Resolution{}, err
	}
	return m[ref.Key()], nil
}
