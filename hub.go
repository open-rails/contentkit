package searchkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/searchkit/search"
	"github.com/open-rails/searchkit/signal"
)

// ErrSignalPlaneDisabled is returned by signal/discovery methods when the hub
// was constructed without a ClickHouse connection.
var ErrSignalPlaneDisabled = errors.New("searchkit: signal plane disabled (no ClickHouse configured)")

// EntityCatalog supplies the entity "universe" for Unseen: live, non-deleted
// entity ids of a type, read from the host's own tables. The host owns
// visibility and gating (premium, region, ...) — searchkit never interprets
// them. Order defines Unseen order (recommended: newest first).
type EntityCatalog interface {
	Universe(ctx context.Context, tenant string, entityType string, q CatalogQuery) ([]string, error)
}

// CatalogQuery bounds a Universe read. Limit 0 = host-defined default.
type CatalogQuery struct {
	Limit int
}

// EntityCatalogFunc adapts a function to the EntityCatalog interface.
type EntityCatalogFunc func(ctx context.Context, tenant string, entityType string, q CatalogQuery) ([]string, error)

func (f EntityCatalogFunc) Universe(ctx context.Context, tenant string, entityType string, q CatalogQuery) ([]string, error) {
	return f(ctx, tenant, entityType, q)
}

// Hub is the single surface host apps program against: content-plane queries
// (search/typeahead/similar), the signal plane (RecordSignal), and the
// discovery plane (reads over entities × signals). All methods return ranked
// entity ids (+ per-user State); the host hydrates ids → cards from its own
// DB. Every method is implicitly tenant-scoped (pinned at construction in
// embedded mode).
type Hub interface {
	// Tenant returns the tenant this hub instance is scoped to.
	Tenant() string

	// Content plane.
	Search(ctx context.Context, userText string, opts HubSearchOptions) (SearchResult, error)
	Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error)
	SimilarTo(ctx context.Context, entityType, entityID string, opts HubSimilarOptions) ([]RecHit, error)

	// Signal plane.
	RecordSignals(ctx context.Context, signals []signal.Signal) error
	RecordExposures(ctx context.Context, exposures []signal.Exposure) error
	ForgetExposures(ctx context.Context, subject signal.Subject) error
	Attribution(ctx context.Context, opts signal.AttributionOptions) (signal.AttributionPage, error)
	Forget(ctx context.Context, subject signal.Subject, entityType, entityID string) error
	EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error)
	EnforceErasures(ctx context.Context) (signal.ErasureReport, error)

	// Discovery plane.
	History(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) ([]signal.StateRow, error)
	HistoryCount(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) (int64, error)
	SeenIDs(ctx context.Context, subject signal.Subject, entityType string) (map[string]struct{}, error)
	Unseen(ctx context.Context, subject signal.Subject, opts UnseenOptions) ([]string, error)
	States(ctx context.Context, subject signal.Subject, refs []signal.EntityRef) (map[signal.EntityRef]signal.State, error)
	Metrics(ctx context.Context, entityType string, ids []string, window signal.Window) (map[string]signal.EntityMetrics, error)
	Popular(ctx context.Context, entityType string, opts signal.PopularOptions) ([]signal.PopularHit, error)
	PopularityFor(ctx context.Context, entityType string, ids []string, window signal.Window) (map[string]float64, error)
	Recommend(ctx context.Context, subject signal.Subject, opts RecommendOptions) ([]RecHit, error)

	// Maintenance.
	RefreshCoEngagement(ctx context.Context, opts signal.RefreshCoEngagementOptions) error
	RepairProjections(ctx context.Context, opts signal.RepairOptions) (signal.RepairResult, error)
	Inventory(ctx context.Context) ([]signal.InventoryRow, error)
	PurgeEntityTypes(ctx context.Context, entityTypes []string) error
}

// EmbeddedHub implements the full Hub surface; a future remote client must
// too (cross-mode conformance, see open-rails-tracker/searchkit/future.md). Embedded-only
// extras like Client() are deliberately NOT part of the interface.
var _ Hub = (*EmbeddedHub)(nil)

// EmbeddedConfig configures an in-process hub against the shared DB.
type EmbeddedConfig struct {
	// Content plane (Postgres). PG + PGSchema are required. PGSchema should
	// be a dedicated schema (not the host app's schema) to avoid table
	// collisions.
	PG       *pgxpool.Pool
	PGSchema string
	Embedder Embedder

	// Content-plane defaults (as in ClientConfig).
	DefaultLanguage  string
	DefaultModel     string
	DefaultLimit     int
	DefaultRRFK      int
	TwoStage         bool
	OversampleFactor int

	// Signal plane (ClickHouse). Optional: omit CH to run content-only
	// (signal/discovery methods return ErrSignalPlaneDisabled). CHDatabase
	// is the hub's dedicated ClickHouse database; apply
	// migrations.SignalClickHouse and gate startup on signal.CheckSchema.
	CH         signal.Conn
	CHDatabase string

	// Tenant is the single implicit tenant for this embedded hub. Defaults
	// to "default". The tenant column exists so embedded and (future)
	// multi-tenant server mode share storage and code.
	Tenant string

	// Scorers maps entity type → host Scorer. When a signal arrives for a
	// registered type, the scorer's result overwrites Score / Progress /
	// ProgressMax / Completed before recording.
	Scorers map[string]signal.Scorer

	// Catalogs maps entity type → host EntityCatalog (the Unseen universe).
	Catalogs map[string]EntityCatalog
}

// EmbeddedHub implements Hub in-process. Construct with NewEmbedded.
type EmbeddedHub struct {
	client   *Client
	store    *signal.Store
	tenant   string
	scorers  map[string]signal.Scorer
	catalogs map[string]EntityCatalog
}

var _ Hub = (*EmbeddedHub)(nil)

// NewEmbedded builds the embedded hub: in-process, shared DB, single implicit
// tenant.
func NewEmbedded(cfg EmbeddedConfig) (*EmbeddedHub, error) {
	client, err := NewClient(ClientConfig{
		Pool:             cfg.PG,
		Schema:           cfg.PGSchema,
		Embedder:         cfg.Embedder,
		DefaultLanguage:  cfg.DefaultLanguage,
		DefaultModel:     cfg.DefaultModel,
		DefaultLimit:     cfg.DefaultLimit,
		DefaultRRFK:      cfg.DefaultRRFK,
		TwoStage:         cfg.TwoStage,
		OversampleFactor: cfg.OversampleFactor,
	})
	if err != nil {
		return nil, err
	}

	h := &EmbeddedHub{
		client:   client,
		tenant:   strings.TrimSpace(cfg.Tenant),
		scorers:  cfg.Scorers,
		catalogs: cfg.Catalogs,
	}
	if h.tenant == "" {
		h.tenant = "default"
	}
	if cfg.CH != nil {
		store, err := signal.NewStore(cfg.CH, cfg.CHDatabase)
		if err != nil {
			return nil, err
		}
		h.store = store
	}
	return h, nil
}

// Client returns the underlying content-plane client (advanced use).
func (h *EmbeddedHub) Client() *Client { return h.client }

// Tenant returns the pinned tenant value.
func (h *EmbeddedHub) Tenant() string { return h.tenant }

func (h *EmbeddedHub) requireStore() (*signal.Store, error) {
	if h.store == nil {
		return nil, ErrSignalPlaneDisabled
	}
	return h.store, nil
}

// --- Content plane ---

// Personalization fuses signal aggregates into search ranking. Recall is
// unchanged — this is a ranking-only layer over the candidate set. A
// per-request toggle the host flips (e.g. only for logged-in users).
type Personalization struct {
	Subject signal.Subject

	// PopularityWeight is the RRF weight of the candidate-set popularity
	// list blended with the content ranking. Defaults to 0.25.
	PopularityWeight float32
	// PopularityWindow bounds candidate popularity (zero = all time).
	PopularityWindow signal.Window

	// AffinityWeight boosts entities the subject already engaged with by
	// (1 + AffinityWeight·last_score/100). 0 = off.
	AffinityWeight float32

	// DemoteSeen demotes already-seen / completed entities.
	DemoteSeen bool
	// SeenPenalty multiplies seen-but-not-completed scores (default 0.85).
	SeenPenalty float32
	// CompletedPenalty multiplies completed scores (default 0.6).
	CompletedPenalty float32
	// DislikePenalty multiplies entities the subject has net-negative
	// explicit feedback for (default 0.3). Always applied when view context
	// is loaded (i.e. AffinityWeight > 0 or DemoteSeen).
	DislikePenalty float32
}

// HubSearchOptions extends content SearchOptions with optional
// signal-aware personalization.
type HubSearchOptions struct {
	SearchOptions
	Personalize *Personalization
}

func (h *EmbeddedHub) Search(ctx context.Context, userText string, opts HubSearchOptions) (SearchResult, error) {
	if opts.Personalize == nil {
		return h.client.Search(ctx, userText, opts.SearchOptions)
	}
	store, err := h.requireStore()
	if err != nil {
		return SearchResult{}, err
	}
	p := *opts.Personalize
	if err := p.Subject.Validate(); err != nil {
		return SearchResult{}, err
	}
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

	// Oversample the content ranking so re-ranking has headroom; the page is
	// cut from the re-ranked list.
	base := opts.SearchOptions
	base.Offset = 0
	base.Limit = clampInt((offset+limit)*3, 50, 500)
	content, err := h.client.Search(ctx, userText, base)
	if err != nil {
		return SearchResult{}, err
	}
	hits := content.Hits
	if len(hits) == 0 {
		return content, nil
	}

	// Candidate popularity, per entity type.
	idsByType := map[string][]string{}
	for _, hit := range hits {
		idsByType[hit.EntityType] = append(idsByType[hit.EntityType], hit.EntityID)
	}
	popularity := map[signal.EntityRef]float64{}
	for t, ids := range idsByType {
		scores, err := store.PopularityFor(ctx, h.tenant, t, ids, p.PopularityWindow)
		if err != nil {
			return SearchResult{}, err
		}
		for id, s := range scores {
			popularity[signal.EntityRef{EntityType: t, EntityID: id}] = s
		}
	}

	// RRF-fuse the content list with the candidate popularity list.
	byKey := make(map[search.RRFKey]SearchHit, len(hits))
	contentList := make([]search.RRFKey, 0, len(hits))
	for _, hit := range hits {
		key := search.RRFKey{EntityType: hit.EntityType, EntityID: hit.EntityID, Language: hit.Language}
		byKey[key] = hit
		contentList = append(contentList, key)
	}
	popList := make([]search.RRFKey, 0, len(hits))
	for _, hit := range hits {
		if popularity[signal.EntityRef{EntityType: hit.EntityType, EntityID: hit.EntityID}] > 0 {
			popList = append(popList, search.RRFKey{EntityType: hit.EntityType, EntityID: hit.EntityID, Language: hit.Language})
		}
	}
	sort.SliceStable(popList, func(i, j int) bool {
		pi := popularity[signal.EntityRef{EntityType: popList[i].EntityType, EntityID: popList[i].EntityID}]
		pj := popularity[signal.EntityRef{EntityType: popList[j].EntityType, EntityID: popList[j].EntityID}]
		return pi > pj
	})
	fused := search.FuseRRF([][]search.RRFKey{contentList, popList}, search.RRFOptions{
		K:       h.client.defaultRRFK,
		Weights: []float32{1, p.PopularityWeight},
	})

	// Per-subject view context: affinity boost + seen/completed demotion.
	var states map[signal.EntityRef]signal.State
	if p.AffinityWeight > 0 || p.DemoteSeen {
		refs := make([]signal.EntityRef, 0, len(hits))
		for _, hit := range hits {
			refs = append(refs, signal.EntityRef{EntityType: hit.EntityType, EntityID: hit.EntityID})
		}
		states, err = store.States(ctx, h.tenant, p.Subject, refs)
		if err != nil {
			return SearchResult{}, err
		}
	}

	out := make([]SearchHit, 0, len(fused))
	for _, f := range fused {
		score := f.Score
		if st, ok := states[signal.EntityRef{EntityType: f.EntityType, EntityID: f.EntityID}]; ok {
			if st.NetValue < 0 {
				// Explicit negative feedback outranks every other adjustment.
				score *= p.DislikePenalty
			} else {
				if p.AffinityWeight > 0 && st.LastScore > 0 {
					score *= 1 + p.AffinityWeight*float32(st.LastScore)/100
				}
				if p.DemoteSeen {
					switch {
					case st.Completed:
						score *= p.CompletedPenalty
					case st.Seen:
						score *= p.SeenPenalty
					}
				}
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
		if out[i].EntityType != out[j].EntityType {
			return out[i].EntityType < out[j].EntityType
		}
		return out[i].EntityID < out[j].EntityID
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

func (h *EmbeddedHub) Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error) {
	return h.client.Typeahead(ctx, userText, opts)
}

// RecHit is one ranked entity from fused similarity or recommendations.
type RecHit struct {
	EntityType string
	EntityID   string
	Score      float32
}

// HubSimilarOptions extends content SimilarOptions with co-engagement fusion:
// "more like this" = vector similarity ⊕ subjects-who-engaged-X-also-engaged-Y.
type HubSimilarOptions struct {
	SimilarOptions

	// CoEngagement enables fusing the co-engagement list (requires the
	// signal plane).
	CoEngagement bool
	// CoEngagementWeight is the RRF weight of the co-engagement list
	// relative to the vector list (default 1).
	CoEngagementWeight float32
	// CoEngagementWindow bounds the co-engagement scan (default all time).
	CoEngagementWindow signal.Window

	// ExcludeSeenFor drops entities this subject has already seen (and
	// always drops entities they negatively reacted to).
	ExcludeSeenFor *signal.Subject

	// DiversityLambda enables MMR diversity over the fused list ((0,1),
	// higher = more relevance). 0 disables.
	DiversityLambda float32
}

func (h *EmbeddedHub) SimilarTo(ctx context.Context, entityType, entityID string, opts HubSimilarOptions) ([]RecHit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = h.client.defaultLimit
	}

	lists := make([][]search.RRFKey, 0, 2)
	weights := make([]float32, 0, 2)

	// Vector similarity (skipped when no model is configured — e.g. a
	// lexical-only deployment — so co-engagement can still serve).
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = h.client.defaultModel
	}

	// The two sources are independent and live in different stores, so they run
	// together; the fused order is unchanged because the lists are appended
	// afterwards in their original order, and RRF weights are positional.
	var (
		vectorKeys []search.RRFKey
		coKeys     []search.RRFKey
	)
	group, groupCtx := errgroup.WithContext(ctx)
	if model != "" {
		group.Go(func() error {
			simOpts := opts.SimilarOptions
			simOpts.Limit = clampInt(limit*2, limit, 200)
			vec, err := h.client.SimilarTo(groupCtx, entityType, entityID, simOpts)
			if err != nil {
				return err
			}
			keys := make([]search.RRFKey, 0, len(vec))
			for _, v := range vec {
				keys = append(keys, search.RRFKey{EntityType: v.EntityType, EntityID: v.EntityID})
			}
			vectorKeys = keys
			return nil
		})
	}
	if opts.CoEngagement {
		store, err := h.requireStore()
		if err != nil {
			return nil, err
		}
		group.Go(func() error {
			co, err := store.CoEngaged(groupCtx, h.tenant, signal.EntityRef{EntityType: entityType, EntityID: entityID}, signal.CoEngagedOptions{
				EntityTypes: opts.EntityTypes,
				Window:      opts.CoEngagementWindow,
				Limit:       clampInt(limit*2, limit, 200),
			})
			if err != nil {
				return err
			}
			keys := make([]search.RRFKey, 0, len(co))
			for _, c := range co {
				keys = append(keys, search.RRFKey{EntityType: c.EntityType, EntityID: c.EntityID})
			}
			coKeys = keys
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	if model != "" {
		lists = append(lists, vectorKeys)
		weights = append(weights, 1)
	}
	if opts.CoEngagement {
		w := opts.CoEngagementWeight
		if w <= 0 {
			w = 1
		}
		lists = append(lists, coKeys)
		weights = append(weights, w)
	}

	if len(lists) == 0 {
		return nil, fmt.Errorf("searchkit: SimilarTo requires a semantic model or CoEngagement enabled")
	}

	fused := search.FuseRRF(lists, search.RRFOptions{K: h.client.defaultRRFK, Weights: weights})

	var seen map[string]map[string]struct{}
	if opts.ExcludeSeenFor != nil {
		store, err := h.requireStore()
		if err != nil {
			return nil, err
		}
		seen = map[string]map[string]struct{}{}
		for _, f := range fused {
			if _, ok := seen[f.EntityType]; ok {
				continue
			}
			s, err := store.SeenIDs(ctx, h.tenant, *opts.ExcludeSeenFor, f.EntityType)
			if err != nil {
				return nil, err
			}
			seen[f.EntityType] = s
		}
	}

	var negative map[signal.EntityRef]struct{}
	if opts.ExcludeSeenFor != nil && h.store != nil {
		neg, err := h.store.NegativeIDs(ctx, h.tenant, *opts.ExcludeSeenFor, opts.EntityTypes)
		if err != nil {
			return nil, err
		}
		negative = neg
	}

	candidates := make([]RecHit, 0, len(fused))
	for _, f := range fused {
		if f.EntityType == entityType && f.EntityID == entityID {
			continue // drop the anchor
		}
		if seen != nil {
			if _, ok := seen[f.EntityType][f.EntityID]; ok {
				continue
			}
		}
		if negative != nil {
			if _, ok := negative[signal.EntityRef{EntityType: f.EntityType, EntityID: f.EntityID}]; ok {
				continue
			}
		}
		candidates = append(candidates, RecHit{EntityType: f.EntityType, EntityID: f.EntityID, Score: f.Score})
	}
	if opts.DiversityLambda > 0 {
		candidates = h.diversifyRecHits(ctx, candidates, opts.DiversityLambda, model, opts.Language)
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

// RefreshCoEngagement (re)materializes the item_pairs co-engagement rollup
// for this tenant (see signal.Store.RefreshCoEngagement). Run periodically.
func (h *EmbeddedHub) RefreshCoEngagement(ctx context.Context, opts signal.RefreshCoEngagementOptions) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.RefreshCoEngagement(ctx, h.tenant, opts)
}

// RepairProjections is the bounded, host-scheduled projection repair (see
// signal.Store.RepairProjections): run it periodically with IngestedSince for
// crash repair, and with Rebuild over a window after semantic changes.
func (h *EmbeddedHub) RepairProjections(ctx context.Context, opts signal.RepairOptions) (signal.RepairResult, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.RepairResult{}, err
	}
	return store.RepairProjections(ctx, h.tenant, opts)
}

// Inventory reports canonical event volume per entity and signal type.
func (h *EmbeddedHub) Inventory(ctx context.Context) ([]signal.InventoryRow, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Inventory(ctx, h.tenant)
}

// PurgeEntityTypes irreversibly deletes whole entity types from this tenant's
// signal plane (see signal.Store.PurgeEntityTypes).
func (h *EmbeddedHub) PurgeEntityTypes(ctx context.Context, entityTypes []string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.PurgeEntityTypes(ctx, h.tenant, entityTypes)
}

// RecordExposures logs one row per result list and stage (served, rendered,
// visible) so clicks can be attributed to what was actually exposed. Hosts call
// it once per list per stage, never per item.
func (h *EmbeddedHub) RecordExposures(ctx context.Context, exposures []signal.Exposure) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.RecordExposures(ctx, h.tenant, exposures)
}

// ForgetExposures clears a subject's result-list exposures (search history).
func (h *EmbeddedHub) ForgetExposures(ctx context.Context, subject signal.Subject) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ForgetExposures(ctx, h.tenant, subject)
}

// Attribution exports renders at one stage with their clicks joined (see
// signal.Store.Attribution); the evaluation dataset source.
func (h *EmbeddedHub) Attribution(ctx context.Context, opts signal.AttributionOptions) (signal.AttributionPage, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.AttributionPage{}, err
	}
	return store.Attribution(ctx, h.tenant, opts)
}

// --- Signal plane ---

// RecordSignals applies each entity type's registered Scorer, then records the
// batch (see signal.Store.RecordSignals). A scorer error records nothing.
func (h *EmbeddedHub) RecordSignals(ctx context.Context, signals []signal.Signal) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	out := make([]signal.Signal, 0, len(signals))
	for _, s := range signals {
		if scorer, ok := h.scorers[s.EntityType]; ok && scorer != nil {
			scored, err := scorer.Score(ctx, s)
			if err != nil {
				return fmt.Errorf("searchkit: scorer for %q: %w", s.EntityType, err)
			}
			s.Score = scored.Score
			s.Progress = scored.Progress
			s.ProgressMax = scored.ProgressMax
			s.Completed = scored.Completed
		}
		out = append(out, s)
	}
	return store.RecordSignals(ctx, h.tenant, out)
}

// --- Discovery plane ---

func (h *EmbeddedHub) History(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) ([]signal.StateRow, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.History(ctx, h.tenant, subject, opts)
}

// Forget erases the subject's signals for one entity (entityID set) or a
// whole entity type (entityID empty) — host "clear my history" support.
func (h *EmbeddedHub) Forget(ctx context.Context, subject signal.Subject, entityType, entityID string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.Forget(ctx, h.tenant, subject, entityType, entityID)
}

// EraseSubjects permanently erases subjects from this tenant's signal plane:
// see signal.Store.EraseSubjects for the completion contract. Shared accounts
// exist in several tenants: each host erases its own tenant.
func (h *EmbeddedHub) EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.ErasureReport{}, err
	}
	return store.EraseSubjects(ctx, []string{h.tenant}, subjects)
}

// EnforceErasures physically removes residue of every recorded erasure of
// this tenant (see signal.Store.EnforceErasures): schedule it and run it after
// every restore.
func (h *EmbeddedHub) EnforceErasures(ctx context.Context) (signal.ErasureReport, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.ErasureReport{}, err
	}
	return store.EnforceErasures(ctx, h.tenant)
}

// HistoryCount returns the total row count History would paginate over.
func (h *EmbeddedHub) HistoryCount(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) (int64, error) {
	store, err := h.requireStore()
	if err != nil {
		return 0, err
	}
	return store.HistoryCount(ctx, h.tenant, subject, opts)
}

// SeenIDs returns the subject's seen-set for one entity type (the signal-plane
// half of the unseen anti-join). Use when the host wants to run its own diff
// against a custom-filtered universe instead of Unseen's registered catalog.
func (h *EmbeddedHub) SeenIDs(ctx context.Context, subject signal.Subject, entityType string) (map[string]struct{}, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.SeenIDs(ctx, h.tenant, subject, entityType)
}

// UnseenOptions controls Unseen reads.
type UnseenOptions struct {
	// EntityType selects which catalog universe to diff against. Required.
	EntityType string
	// Limit caps the returned ids (default 50). Order follows the host
	// catalog's Universe order.
	Limit int
	// CatalogLimit is passed through to the host catalog's Universe call
	// (0 = host default).
	CatalogLimit int
}

// Unseen returns catalog ids the subject has not seen (max_progress > 0
// defines "seen"): host universe MINUS the subject's seen-set. The host
// catalog applies its own visibility/premium gating against its own tables.
func (h *EmbeddedHub) Unseen(ctx context.Context, subject signal.Subject, opts UnseenOptions) ([]string, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	entityType := strings.TrimSpace(opts.EntityType)
	if entityType == "" {
		return nil, fmt.Errorf("searchkit: UnseenOptions.EntityType is required")
	}
	catalog, ok := h.catalogs[entityType]
	if !ok || catalog == nil {
		return nil, fmt.Errorf("searchkit: no EntityCatalog registered for entity type %q", entityType)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	universe, err := catalog.Universe(ctx, h.tenant, entityType, CatalogQuery{Limit: opts.CatalogLimit})
	if err != nil {
		return nil, fmt.Errorf("searchkit: catalog universe for %q: %w", entityType, err)
	}
	seen, err := store.SeenIDs(ctx, h.tenant, subject, entityType)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, limit)
	for _, id := range universe {
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (h *EmbeddedHub) States(ctx context.Context, subject signal.Subject, refs []signal.EntityRef) (map[signal.EntityRef]signal.State, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.States(ctx, h.tenant, subject, refs)
}

func (h *EmbeddedHub) Popular(ctx context.Context, entityType string, opts signal.PopularOptions) ([]signal.PopularHit, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Popular(ctx, h.tenant, entityType, opts)
}

// Metrics returns named window metrics (viewers, views, completions,
// feedback, ...) for entity ids of one type.
func (h *EmbeddedHub) Metrics(ctx context.Context, entityType string, ids []string, window signal.Window) (map[string]signal.EntityMetrics, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Metrics(ctx, h.tenant, entityType, ids, window)
}

// PopularityFor scores a fixed candidate set (entity ids of one type) by the
// popularity ranking, returning entity_id -> score. Use to rank a host-
// filtered universe (e.g. "galleries of artist X by popularity").
func (h *EmbeddedHub) PopularityFor(ctx context.Context, entityType string, ids []string, window signal.Window) (map[string]float64, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.PopularityFor(ctx, h.tenant, entityType, ids, window)
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
