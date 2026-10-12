// Package resolve is how ContentKit's modules ask whether content exists, is
// visible, opens and is edited by a viewer. The catalog and groups answer it;
// media, versions, interactions and discovery ask it. It is internal: hosts
// set access through the API, never through a resolver.
package resolve

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// Resolution is the verdict about a content reference for one actor.
type Resolution struct {
	// Ref is the canonical reference every row is stored and read under (a
	// merged group resolves to its survivor). A zero Ref keeps the requested
	// one; a Ref of another tenant is an error.
	Ref contentref.ContentRef
	// Visible = live, or edited by the actor. Media's public files (covers,
	// previews) need only Visible.
	Visible bool
	// Accessible = the actor opens it. For media it is all or nothing: every
	// private file of the item, or none.
	Accessible bool
	// Editor = the actor may edit the item (staff, its creators, its owner
	// group's editors): media gives a visible item's editor its private files
	// whatever Accessible says.
	Editor bool
	// Owner is the user that owns the item (its first creator, a group's
	// owner), "" when none does. A comment ban in that owner's scope applies
	// to it.
	Owner string
	// Comments is the item's comments setting: open (also ""), closed or
	// hidden.
	Comments string
}

// Full reports access to the item: everything private is served.
func (r Resolution) Full() bool {
	return r.Visible && r.Accessible
}

// Resolver says whether each ContentRef exists, is visible and opens for the
// actor. It is batch-first: callers pass every ref a request needs in one
// call. The map is keyed by each requested ref's Key; a ref missing from it
// denies. An error fails the whole batch.
type Resolver interface {
	Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]Resolution, error)
}

// One resolves a single ref as a batch of one; an omitted ref yields the
// zero (denying) Resolution.
func One(ctx context.Context, r Resolver, ref contentref.ContentRef, actor access.Actor) (Resolution, error) {
	m, err := r.Resolve(ctx, []contentref.ContentRef{ref}, actor)
	if err != nil {
		return Resolution{}, err
	}
	return m[ref.Key()], nil
}
