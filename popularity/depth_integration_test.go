package popularity_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// depthSource is the real hub, recording the ranking depth of every read.
type depthSource struct {
	popularity.Source
	mu     sync.Mutex
	limits []int
}

func (s *depthSource) Popular(ctx context.Context, kind string, opts signal.PopularOptions) ([]signal.PopularHit, error) {
	s.mu.Lock()
	s.limits = append(s.limits, opts.Limit)
	s.mu.Unlock()
	return s.Source.Popular(ctx, kind, opts)
}

// Request-chosen offsets cannot make one read return, or the cache hold, more
// than MaxDepth ranked works, nor grow the number of cached rankings: every
// offset of a sweep shares one of two power-of-two prefixes, and the listing
// ends at MaxDepth with the same order a shallow page sees.
func TestIntegrationPopularDepthIsBounded(t *testing.T) {
	ctx := context.Background()
	env := signaltest.FromEnv(t)
	const db, works, maxDepth = "contentkit_popularity_depth_test", 150, 100
	hub := newHub(t, env.Fresh(t, db), db, fxTenant, popularity.SessionScorer{SecondsPerUnit: 8})
	at := time.Now().UTC().Add(-time.Hour)
	var views []signal.Signal
	for w := 0; w < works; w++ {
		for v := 0; v <= w%7; v++ {
			views = append(views, signal.Signal{ContentRef: ref(cid(900000 + w)), Subject: signal.Subject{UserID: fmt.Sprintf("depth-%d-%d", w, v)},
				Type: signal.TypeView, EventID: fmt.Sprintf("depth-%d-%d", w, v), OccurredAt: at, DurationS: 60, Progress: 10, ProgressMax: 10})
		}
	}
	recordBatched(t, hub, views)

	source := &depthSource{Source: hub}
	cache := popularity.NewMemoryCache(0)
	ranker, err := popularity.New(popularity.Config{Source: source, Policy: popularity.PolicyV1, Cache: cache, MaxDepth: maxDepth, TaxonomyCandidateLimit: maxDepth})
	if err != nil {
		t.Fatal(err)
	}
	window := signal.AllTime()
	full, err := ranker.Popular(ctx, fxKind, window, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != maxDepth {
		t.Fatalf("a limit past MaxDepth returned %d works, want exactly %d", len(full), maxDepth)
	}
	for offset := 0; offset <= 50_000; offset += 13 {
		page, err := ranker.Popular(ctx, fxKind, window, offset+20)
		if err != nil {
			t.Fatal(err)
		}
		want := full[:min(offset+20, maxDepth)]
		if !slices.EqualFunc(page, want, func(a, b popularity.Hit) bool { return a.ContentID == b.ContentID && a.Score == b.Score }) {
			t.Fatalf("offset %d: the bounded prefix differs from the full ranking's", offset)
		}
	}
	if !slices.Equal(source.limits, []int{maxDepth, 64}) {
		t.Fatalf("ClickHouse reads %v, want one read per prefix [%d 64] across the whole sweep", source.limits, maxDepth)
	}
	if cache.Len() != 2 {
		t.Fatalf("the sweep left %d cached rankings, want 2", cache.Len())
	}
}
