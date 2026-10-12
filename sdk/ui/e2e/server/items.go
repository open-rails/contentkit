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
