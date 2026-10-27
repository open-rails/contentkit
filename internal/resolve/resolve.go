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
	// Accessible = the actor opens the content. MediaFiles may further limit
	// which immutable private objects a media read can expose.
	Accessible bool
	// ReadRestricted allows private reads before publication or chapter release.
	// It is granted by the content authority; it never permits uploads or edits.
	ReadRestricted bool
	// MediaFiles limits private media to these exact immutable blob references
	// from the published snapshot. Nil leaves ordinary whole-item access
	// unchanged; a non-nil empty map denies every private file. Publishing
	// kinds require this allowlist without Editor/ReadRestricted authority, even
	// when Accessible is true.
	// This is filled only by trusted resolvers, never a read request.
	MediaFiles map[string]bool
	// Editor = the actor may edit the item (current staff, authorized authors, its owner
	// group's editors): media gives a visible item's editor its private files
	// whatever Accessible says.
	Editor bool
	// Owner is the account that authored the item (immutable created_by, an entity's
	// owner), "" when none does. A comment ban in that owner's scope applies
	// to it.
	Owner string
	// Comments is the item's comments setting: open (also ""), closed or
	// hidden.
	Comments string
	// Channel names the primary authoring home whose current moderators manage
	// this item and all its versions. Repost destinations never replace it.
	Channel *Channel
}

// Channel is the stored primary channel identity and its permission scope.
type Channel struct {
	Kind, ID, Owner, Scope string
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

type editorReadKey struct{}

// WithEditorRead marks an explicit media management read. It grants no
// authority: resolvers must still verify that the actor edits the resource.
func WithEditorRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, editorReadKey{}, true)
}

// EditorRead reports a media read that explicitly requested its editor view.
func EditorRead(ctx context.Context) bool {
	want, _ := ctx.Value(editorReadKey{}).(bool)
	return want
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
