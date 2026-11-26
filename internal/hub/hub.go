package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"

	signalstore "github.com/open-rails/contentkit/internal/signalstore"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/search"
)

var (
	// ErrSignalPlaneDisabled is returned by signal methods while the hub has
	// no signal plane: none is configured, or it is down (ErrSignalPlaneDown).
	ErrSignalPlaneDisabled = errors.New("contentkit: signal plane disabled")
	// ErrSignalPlaneDown: the configured signal plane is unreachable until
	// SetSignals opens it again.
	ErrSignalPlaneDown = fmt.Errorf("%w: ClickHouse is down", ErrSignalPlaneDisabled)
)

// Config configures one tenant's hub.
type Config struct {
	// Content plane (Postgres). PG + PGSchema (ContentKit's schema) are required.
	PG       *pgxpool.Pool
	PGSchema string

	DefaultLanguage string

	// SignalPlane: a signal plane is configured that SetSignals opens later;
	// until then signal methods return ErrSignalPlaneDown. Without it signal
	// methods return ErrSignalPlaneDisabled.
	SignalPlane bool

	// Tenant is the single tenant of this embedded hub. Required: every
	// document, signal, cursor and result carries it.
	Tenant string
}

// Hub is one tenant's search client and signal plane: the engine's internal
// surface the discovery routes build on.
type Hub struct {
	client     *Client
	store      atomic.Pointer[signalstore.Store]
	configured bool
	tenant     string
}

// searchRRFK is the fusion constant of personalized search.
const searchRRFK = 60

// New builds the hub of one tenant.
func New(cfg Config) (*Hub, error) {
	client, err := NewClient(ClientConfig{
		Pool:            cfg.PG,
		Schema:          cfg.PGSchema,
		Tenant:          cfg.Tenant,
		DefaultLanguage: cfg.DefaultLanguage,
	})
	if err != nil {
		return nil, err
	}
	return &Hub{client: client, tenant: client.Tenant(), configured: cfg.SignalPlane}, nil
}

// Content returns a reference to a work of this hub's tenant.
func (h *Hub) Content(contentKind, contentID string) ContentRef {
	return contentref.New(h.tenant, contentKind, contentID)
}

func (h *Hub) requireStore() (*signalstore.Store, error) {
	if s := h.store.Load(); s != nil {
		return s, nil
	}
	if h.configured {
		return nil, ErrSignalPlaneDown
	}
	return nil, ErrSignalPlaneDisabled
}

// --- Content plane ---

// Personalization adjusts search ranking for one reader. Recall is
// unchanged: a ranking-only layer over the candidate set.
type Personalization struct {
	// States is the reader's standing with candidate works, from their
	// reading state; works absent from the map are unknown to them.
	States func(ctx context.Context, refs []ContentRef) (map[ContentKey]ViewState, error)

	// PopularityWeight is the RRF weight of the candidate-set popularity
	// list blended with the content ranking. Defaults to 0.25.
	PopularityWeight float32
	// Popularity scores candidate works of one kind, higher first; works
	// without a score are absent. Nil leaves popularity out.
	Popularity func(ctx context.Context, kind string, ids []string) (map[string]float64, error)

	// DemoteSeen demotes already-seen / completed works.
	DemoteSeen bool
	// SeenPenalty multiplies seen-but-not-completed scores (default 0.85).
	SeenPenalty float32
	// CompletedPenalty multiplies completed scores (default 0.6).
	CompletedPenalty float32
	// DislikePenalty multiplies works the reader disliked (default 0.3).
	DislikePenalty float32
}

// ViewState is a reader's standing with one work.
type ViewState struct {
	Seen, Completed, Disliked bool
}

// HubSearchOptions extends content SearchOptions with optional
// signal-aware personalization.
type HubSearchOptions struct {
	SearchOptions
	Personalize *Personalization
}

// KeywordMatch is the SQL selecting every content item matching userText; see
// Client.KeywordMatch.
func (h *Hub) KeywordMatch(ctx context.Context, userText string, opts SearchOptions) (search.Match, error) {
	return h.client.KeywordMatch(ctx, userText, opts)
}

func (h *Hub) Search(ctx context.Context, userText string, opts HubSearchOptions) (SearchResult, error) {
	if opts.Personalize == nil {
		return h.client.Search(ctx, userText, opts.SearchOptions)
	}
	p := *opts.Personalize
	if p.PopularityWeight <= 0 {
		p.PopularityWeight = 0.25
	}
	if p.SeenPenalty <= 0 {
		p.SeenPenalty = 0.85
	}
	if p.CompletedPenalty <= 0 {
		p.CompletedPenalty = 0.6
	}
	if p.DislikePenalty <= 0 {
		p.DislikePenalty = 0.3
	}

	limit, offset, _ := h.client.effectiveLimits(opts.SearchOptions)

	groups, truncated, err := h.client.searchGroups(ctx, userText, opts.SearchOptions)
	if err != nil {
		return SearchResult{}, err
	}
	content := SearchResult{Hits: make([]SearchHit, 0, len(groups)), Truncated: truncated, HasMore: truncated}
	for _, g := range groups {
		content.Hits = append(content.Hits, hitFromGroup(g))
	}
	hits := content.Hits
	if len(hits) == 0 {
		return content, nil
	}

	// Candidate popularity per kind, at work level.
	idsByKind := map[string][]string{}
	for _, hit := range hits {
		idsByKind[hit.ContentKind] = append(idsByKind[hit.ContentKind], hit.ContentID)
	}
	popularity := map[ContentKey]float64{}
	for kind, ids := range idsByKind {
		scores := map[string]float64{}
		if p.Popularity != nil {
			if scores, err = p.Popularity(ctx, kind, ids); err != nil {
				return unpersonalized(ctx, content, offset, limit, err), nil
			}
		}
		for id, s := range scores {
			popularity[h.Content(kind, id).Key()] = s
		}
	}

	// RRF-fuse the content list with the candidate popularity list.
	byKey := make(map[search.RRFKey]SearchHit, len(hits))
	contentList := make([]search.RRFKey, 0, len(hits))
	for _, hit := range hits {
		key := search.RRFKey{ContentKey: hit.Key(), Language: hit.Language}
		byKey[key] = hit
		contentList = append(contentList, key)
	}
	popList := make([]search.RRFKey, 0, len(hits))
	for _, hit := range hits {
		if popularity[hit.Content().Key()] > 0 {
			popList = append(popList, search.RRFKey{ContentKey: hit.Key(), Language: hit.Language})
		}
	}
	sort.SliceStable(popList, func(i, j int) bool {
		return popularity[popList[i].Ref().Content().Key()] > popularity[popList[j].Ref().Content().Key()]
	})
	fused := search.FuseRRF([][]search.RRFKey{contentList, popList}, search.RRFOptions{
		K:       searchRRFK,
		Weights: []float32{1, p.PopularityWeight},
	})

	// The reader's standing: dislike and seen/completed demotion.
	var states map[ContentKey]ViewState
	if p.States != nil {
		refs := make([]ContentRef, 0, len(hits))
		for _, hit := range hits {
			refs = append(refs, hit.Content())
		}
		states, err = p.States(ctx, refs)
		if err != nil {
			return unpersonalized(ctx, content, offset, limit, err), nil
		}
	}

	out := make([]SearchHit, 0, len(fused))
	for _, f := range fused {
		score := f.Score
		if st, ok := states[f.Ref().Content().Key()]; ok {
			switch {
			case st.Disliked:
				// Explicit negative feedback outranks every other adjustment.
				score *= p.DislikePenalty
			case p.DemoteSeen && st.Completed:
				score *= p.CompletedPenalty
			case p.DemoteSeen && st.Seen:
				score *= p.SeenPenalty
			}
		}
		hit := byKey[f.RRFKey]
		hit.Score = score
		out = append(out, hit)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].ContentKind != out[j].ContentKind {
			return out[i].ContentKind < out[j].ContentKind
		}
		return out[i].ContentID < out[j].ContentID
	})
	result := SearchResult{Hits: []SearchHit{}, Truncated: content.Truncated}
	if offset < len(out) {
		end := min(offset+limit, len(out))
		result.Hits = out[offset:end]
		result.HasMore = end < len(out)
	}
	result.HasMore = result.HasMore || content.HasMore
	return result, nil
}

// unpersonalized pages the complete retrieved content ranking when the signal plane
// cannot personalize it: the signal plane is optional, search is not.
func unpersonalized(ctx context.Context, content SearchResult, offset, limit int, err error) SearchResult {
	slog.WarnContext(ctx, "contentkit: search personalization unavailable", "error", err)
	out := SearchResult{Hits: []SearchHit{}, Truncated: content.Truncated, HasMore: content.HasMore}
	if offset < len(content.Hits) {
		end := min(offset+limit, len(content.Hits))
		out.Hits = content.Hits[offset:end]
		out.HasMore = out.HasMore || end < len(content.Hits)
	}
	return out
}

func (h *Hub) Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error) {
	return h.client.Typeahead(ctx, userText, opts)
}

// Signals is the signal plane's store; nil while there is none up.
func (h *Hub) Signals() *signalstore.Store { return h.store.Load() }

// SetSignals opens the configured signal plane over s, or takes it down
// (nil) while ClickHouse is unreachable.
func (h *Hub) SetSignals(s *signalstore.Store) { h.store.Store(s) }

// SignalsConfigured reports whether the hub has a signal plane, up or down.
func (h *Hub) SignalsConfigured() bool { return h.configured }
