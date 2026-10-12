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

// Resolve is versions.Others' resolver for the host's items: a known item is
// visible to everyone unless hidden, open unless locked, and edited by its
// owner and staff; poll images resolve through the content module.
func (h *harness) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := make(map[contentref.ContentKey]access.Resolution, len(refs))
	var staff *bool
	var polls []contentref.ContentRef
	for _, ref := range refs {
		if ref.ContentKind == pollFolder {
			polls = append(polls, ref)
			continue
		}
		it, ok := h.items.get(ref.Key())
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
	if len(polls) > 0 {
		res, err := h.rt.Content.MediaResolver().Resolve(ctx, polls, a)
		if err != nil {
			return nil, err
		}
		for k, r := range res {
			out[k] = r
		}
	}
	return out, nil
}

// CanUpload is versions.Others' upload authorizer: poll images through the
// content module (as hosts wire them), every other item's by its editors.
func (h *harness) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind == pollFolder {
		return h.rt.Content.CanUpload(ctx, a, t)
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
