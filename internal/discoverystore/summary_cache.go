package discoverystore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	discovery "github.com/open-rails/contentkit/discovery"
)

// summaryTTL bounds how long a list's counts may miss a catalog change: an
// edit, a publication or content going live on schedule.
const summaryTTL = time.Minute

const summaryEntries = 4096

// summaryCache keeps a list's selection counts, so its numbered pages and
// repeat views share one count. A key is the compiled selection: its SQL and
// arguments, the viewer's included.
type summaryCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]summaryEntry
}

type summaryEntry struct {
	summary discovery.ListingSummary
	until   time.Time
}

// clockArgs are the selection's arguments that change every request.
var clockArgs = []string{"cv_now", "ci_now", "d_published_now"}

func (s *Service) cachedListingSummary(ctx context.Context, filter Filter, q *sel, sort order) (*discovery.ListingSummary, error) {
	args := maps.Clone(q.args)
	for _, k := range clockArgs {
		delete(args, k)
	}
	raw, err := json.Marshal(struct {
		Predicate, Sort, Join string
		Args                  map[string]any
		SortArgs              map[string]any
	}{q.predicate(), sort.raw, sort.join, args, sort.args})
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256(raw)
	now := s.o.Now()
	if got, ok := s.summaries.get(key, now); ok {
		return &got, nil
	}
	out, unknown, err := s.settledListingSummary(ctx, filter, q, sort, nil)
	if err != nil {
		return nil, err
	}
	if !unknown {
		s.summaries.put(key, *out, now)
	}
	return out, nil
}

func (c *summaryCache) get(key [sha256.Size]byte, now time.Time) (discovery.ListingSummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.until) {
		return discovery.ListingSummary{}, false
	}
	out := e.summary
	out.FilteredEntities = slices.Clone(e.summary.FilteredEntities)
	return out, true
}

func (c *summaryCache) put(key [sha256.Size]byte, summary discovery.ListingSummary, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[[sha256.Size]byte]summaryEntry{}
	}
	if len(c.entries) >= summaryEntries {
		for k, e := range c.entries {
			if !now.Before(e.until) || len(c.entries) >= summaryEntries {
				delete(c.entries, k)
			}
		}
	}
	summary.FilteredEntities = slices.Clone(summary.FilteredEntities)
	c.entries[key] = summaryEntry{summary: summary, until: now.Add(summaryTTL)}
}
