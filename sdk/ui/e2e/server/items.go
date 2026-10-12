package main

import (
	"context"
	"sync"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// item is what the host knows of a content item: who owns it and whether
// viewers may open it. Tests create them at /__test/items.
type item struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Owner  string `json:"owner,omitempty"`
	Access string `json:"access"` // "full", or "none": viewers see it locked
	Hidden bool   `json:"hidden,omitempty"`
	Code   string `json:"code,omitempty"`
	Path   string `json:"path,omitempty"` // its canonical page, when its kind has a route
}

type items struct {
	mu sync.Mutex
	m  map[contentref.ContentKey]item
}

func newItems() *items { return &items{m: map[contentref.ContentKey]item{}} }

func (s *items) get(k contentref.ContentKey) (item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.m[k]
	return it, ok
}

func (s *items) put(k contentref.ContentKey, it item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = it
}

func (s *items) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
}

// Resolve is the host's access.ContentResolver for the items it still owns:
// a known item is visible to everyone unless hidden, open unless locked, and
// edited by its owner and staff. An account's item (kind user) is its
// user's. ContentKit answers for its own post and poll images.
func (h *harness) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := make(map[contentref.ContentKey]access.Resolution, len(refs))
	var staff *bool
	for _, ref := range refs {
		it, ok := h.items.get(ref.Key())
		if !ok && ref.ContentKind == ckUserKind {
			it, ok = item{Owner: ref.ContentID, Access: "full"}, true
		}
		if !ok {
			continue
		}
		editor := !a.Anonymous && a.ID != "" && a.ID == it.Owner
		if !editor && !a.Anonymous {
			if staff == nil {
				s, err := h.editsAllMedia(ctx, a)
				if err != nil {
					return nil, err
				}
				staff = &s
			}
			editor = *staff
		}
		out[ref.Key()] = access.Resolution{Visible: !it.Hidden || editor, Accessible: it.Access != "none" || editor, Owner: it.Owner, Editor: editor}
	}
	return out, nil
}

const ckUserKind = "user"

// CanUpload is media's upload authorizer for the items the host owns: an
// account's media by its person (their quota) or staff (exempt), every
// other item's by its editors.
func (h *harness) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind == ckUserKind {
		if a.Person() && a.ID == t.Ref.ContentID {
			return media.UploadGrant{Allowed: true, Owner: a.ID}, nil
		}
		staff, err := h.editsAllMedia(ctx, a)
		return media.UploadGrant{Allowed: staff, Exempt: staff}, err
	}
	res, err := h.Resolve(ctx, []contentref.ContentRef{t.Ref}, a)
	if err != nil {
		return media.UploadGrant{}, err
	}
	r, ok := res[t.Ref.Key()]
	if !ok || !r.Editor {
		return media.UploadGrant{}, nil
	}
	return media.UploadGrant{Allowed: true, Owner: r.Owner, Exempt: a.ID != r.Owner}, nil
}
