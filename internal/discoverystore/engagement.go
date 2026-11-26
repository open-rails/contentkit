package discoverystore

import (
	"context"
	"errors"

	"github.com/open-rails/contentkit/contentref"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/signal"
)

// seedConcurrency bounds how many seed queries are in flight at once, so one
// recommendation never takes the whole ClickHouse pool.
const seedConcurrency = 4

var errNoStore = errors.New("discovery: Engagement.Store is required")

// query bounds one candidate request.
type query struct {
	// ContentKinds limits candidate kinds (empty = any).
	ContentKinds []string
	// Limit is the number of results the caller returns after filtering.
	Limit int
	// Window bounds the evidence (zero = all time).
	Window signal.Window
	// SeedLimit and SeedContentKinds bound which of the subject's history
	// informs ForSubject (0 / empty = default).
	SeedLimit        int
	SeedContentKinds []string
}

// candidate is one ranked work.
type candidate struct {
	Ref   contentref.ContentRef
	Score float64
}

// Engagement is the co-engagement candidate source: Similar reads works
// co-engaged with the anchor; ForSubject seeds from the subject's
// highest-signal non-negative works and RRF-fuses each seed's co-engaged list.
type Engagement struct {
	Store  *signalstore.Store
	Tenant string
	// RRFK is the seed fusion constant (default 60).
	RRFK int
}

// Similar has no co-engaged candidates: co-engagement read ClickHouse's
// dropped engagement tables.
func (e Engagement) Similar(context.Context, contentref.ContentRef, query) ([]candidate, error) {
	if e.Store == nil {
		return nil, errNoStore
	}
	return nil, nil
}

// ForSubject has no co-engaged candidates for the same reason.
func (e Engagement) ForSubject(context.Context, signal.Subject, query) ([]candidate, error) {
	if e.Store == nil {
		return nil, errNoStore
	}
	return nil, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
