package discovery

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// Hit is one ranked work of a discovery list.
type Hit struct {
	contentref.ContentRef
	Score float32
}

// SimilarOptions controls Similar ("more like this").
type SimilarOptions struct {
	Limit int
	// ContentKinds limits result kinds (default: any).
	ContentKinds []string
	// Window bounds the candidate evidence (default all time).
	Window signal.Window

	// ExcludeSeenFor drops works this subject has already seen (and always
	// drops works they negatively reacted to).
	ExcludeSeenFor *signal.Subject

	// Admit keeps the works the reader may be shown; nil admits all.
	Admit Admit
}

// Admit decides which candidate works a list may show, in one batch: the
// reader's visibility. Recommender oversamples its sources so admission
// still fills the list.
type Admit func(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]bool, error)

// RecommendOptions controls Recommend ("for you": subject → works).
type RecommendOptions struct {
	// ContentKinds are the candidate kinds to recommend. Required.
	ContentKinds []string

	// Limit caps results (default: Recommender.DefaultLimit).
	Limit int

	// SeedLimit is how many of the subject's highest-signal works inform
	// candidates (source default; Engagement: 5).
	SeedLimit int

	// SeedContentKinds limits which kinds may seed (default: any).
	SeedContentKinds []string

	// IncludeSeen keeps already-seen works in results (default: excluded).
	IncludeSeen bool

	// Popular fills cold or thin results from the kind's default prepared bucket.
	// Nil skips popularity fill.
	Popular func(ctx context.Context, kind string, limit int) ([]string, error)

	// Backfill fills what popularity leaves short, from works no signal
	// names; nil leaves the list short.
	Backfill Backfill

	// Admit keeps the works the reader may be shown; nil admits all.
	Admit Admit
}

// Backfill lists up to limit works of kind, best first (e.g. newest), none of
// exclude (content ids): the last fill of a "for you" list.
type Backfill func(ctx context.Context, kind string, exclude []string, limit int) ([]contentref.ContentRef, error)
