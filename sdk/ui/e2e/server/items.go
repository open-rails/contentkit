package main

import (
	"sync"

	"github.com/open-rails/contentkit/contentref"
)

// item is a content item tests create at /__test/items through the
// catalog: its creator (owner), whether viewers may open it (a "none" item
// is pay-per-view) and whether it is published (hidden: a draft).
type item struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Owner  string `json:"owner,omitempty"`
	Access string `json:"access"` // "full", or "none": viewers see it locked
	Hidden bool   `json:"hidden,omitempty"`
	Code   string `json:"code,omitempty"`
	Path   string `json:"path,omitempty"` // its canonical page, when its kind has a route
	// Version is its one version: the item its version-level media live on.
	Version string `json:"version,omitempty"`
}

type items struct {
	mu sync.Mutex
	m  map[contentref.ContentKey]item
}

func newItems() *items { return &items{m: map[contentref.ContentKey]item{}} }

// itemKey is a content item's key: the catalog's item, whatever level its
// kind's media live on.
func itemKey(kind, id string) contentref.ContentKey { return contentref.New(tenant, kind, id).Key() }

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

// versionOwner resolves this harness's fixture identity after storage paths
// stopped carrying the logical parent. A different kind never shares it.
func (s *items) versionOwner(kind, version string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.m {
		if it.Kind == kind && it.Version == version {
			return it.ID
		}
	}
	return ""
}

func (s *items) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
}
