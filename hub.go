package contentkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

// ErrSignalPlaneDisabled is returned by signal/discovery methods when the hub
// was constructed without a ClickHouse connection.
var ErrSignalPlaneDisabled = errors.New("contentkit: signal plane disabled (no ClickHouse configured)")

// ContentCatalog supplies the content "universe" for Unseen: live, non-deleted
// content ids of a kind, read from the host's own tables. The host owns
// visibility and gating (premium, region, ...) — ContentKit never interprets
// them. Order defines Unseen order (recommended: newest first).
type ContentCatalog interface {
	Universe(ctx context.Context, tenant string, contentKind string, q CatalogQuery) ([]string, error)
}

// CatalogQuery bounds a Universe read. Limit 0 = host-defined default.
type CatalogQuery struct {
	Limit int
}

// ContentCatalogFunc adapts a function to the ContentCatalog interface.
type ContentCatalogFunc func(ctx context.Context, tenant string, contentKind string, q CatalogQuery) ([]string, error)

func (f ContentCatalogFunc) Universe(ctx context.Context, tenant string, contentKind string, q CatalogQuery) ([]string, error) {
	return f(ctx, tenant, contentKind, q)
}

// Hub is the single surface host apps program against: content-plane queries
// (search/typeahead), the signal plane (RecordSignals), and the discovery
// plane (reads over content × signals). All methods return ranked content
// references (+ per-subject State); the host hydrates them into cards from
// its own DB. Every method is scoped to the tenant pinned at construction;
// a reference of another tenant is an error.
type Hub interface {
	// Tenant returns the tenant this hub instance is scoped to.
	Tenant() string

	// Content plane.
	Search(ctx context.Context, userText string, opts HubSearchOptions) (SearchResult, error)
	Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error)
	SimilarTo(ctx context.Context, ref ContentRef, opts SimilarOptions) ([]RecHit, error)

	// Signal plane.
	RecordSignals(ctx context.Context, signals []signal.Signal) error
	RecordExposures(ctx context.Context, exposures []signal.Exposure) error
	ForgetExposures(ctx context.Context, subject signal.Subject) error
	ForgetExposuresBefore(ctx context.Context, subject signal.Subject, before time.Time) error
	Attribution(ctx context.Context, opts signal.AttributionOptions) (signal.AttributionPage, error)
	Forget(ctx context.Context, subject signal.Subject, contentKind, contentID string) error
	EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error)
	EnforceErasures(ctx context.Context) (signal.ErasureReport, error)

	// Discovery plane.
	History(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) ([]signal.StateRow, error)
	HistoryCount(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) (int64, error)
	SeenIDs(ctx context.Context, subject signal.Subject, contentKind string) (map[string]struct{}, error)
	Unseen(ctx context.Context, subject signal.Subject, opts UnseenOptions) ([]string, error)
	States(ctx context.Context, subject signal.Subject, refs []ContentRef) (map[ContentKey]signal.State, error)
	Metrics(ctx context.Context, refs []ContentRef, window signal.Window) (map[ContentKey]signal.ContentMetrics, error)
	Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error)
	PopularityFor(ctx context.Context, contentKind string, ids []string, window signal.Window) (map[string]float64, error)
	Recommend(ctx context.Context, subject signal.Subject, opts RecommendOptions) ([]RecHit, error)

	// Maintenance.
	RefreshCoEngagement(ctx context.Context, opts signal.RefreshCoEngagementOptions) error
	RepairProjections(ctx context.Context, opts signal.RepairOptions) (signal.RepairResult, error)
	Inventory(ctx context.Context) ([]signal.InventoryRow, error)
	PurgeContentKinds(ctx context.Context, contentKinds []string) error
}

// EmbeddedConfig configures an in-process hub against the shared DB.
type EmbeddedConfig struct {
	// Content plane (Postgres). PG + PGSchema are required. PGSchema is the
	// host-selected schema containing all ContentKit tables; it may also hold
	// application tables.
	PG       *pgxpool.Pool
	PGSchema string

	// Content-plane defaults (as in ClientConfig).
	DefaultLanguage string
	DefaultLimit    int
	// DefaultRRFK controls deterministic popularity/discovery fusion (default 60).
	DefaultRRFK int

	// Signal plane (ClickHouse). Optional: omit CH to run content-only
	// (signal/discovery methods return ErrSignalPlaneDisabled). CHDatabase
	// is the hub's dedicated ClickHouse database; apply
	// migrations.ClickHouse and gate startup on signal.CheckSchema.
	CH         signal.Conn
	CHDatabase string

	// Tenant is the single tenant of this embedded hub. Required: every
	// document, signal, cursor and result carries it.
	Tenant string

	// Scorers maps content kind → host Scorer. When a signal arrives for a
	// registered kind, the scorer's result overwrites Score / Progress /
	// ProgressMax / Completed before recording.
	Scorers map[string]signal.Scorer

	// Catalogs maps content kind → host ContentCatalog (the Unseen universe).
	Catalogs map[string]ContentCatalog

	// Candidates sources SimilarTo and Recommend (requires CH). Nil =
	// discovery.Engagement (co-engagement) over this hub's signal store.
	Candidates discovery.Candidates
}

// EmbeddedHub implements Hub in-process. Construct with NewEmbedded.
type EmbeddedHub struct {
	defaultRRFK int
	client      *Client
	store       *signal.Store
	tenant      string
	scorers     map[string]signal.Scorer
	catalogs    map[string]ContentCatalog
	discovery   discovery.Recommender
}

var _ Hub = (*EmbeddedHub)(nil)

// NewEmbedded builds the embedded hub: in-process, shared DB, one tenant.
func NewEmbedded(cfg EmbeddedConfig) (*EmbeddedHub, error) {
	client, err := NewClient(ClientConfig{
		Pool:            cfg.PG,
		Schema:          cfg.PGSchema,
		Tenant:          cfg.Tenant,
		DefaultLanguage: cfg.DefaultLanguage,
		DefaultLimit:    cfg.DefaultLimit,
	})
	if err != nil {
		return nil, err
	}
	h := &EmbeddedHub{
		client:      client,
		defaultRRFK: cfg.DefaultRRFK,
		tenant:      client.Tenant(),
		scorers:     cfg.Scorers,
		catalogs:    cfg.Catalogs,
	}
	if h.defaultRRFK <= 0 {
		h.defaultRRFK = 60
	}
	if cfg.CH != nil {
		store, err := signal.NewStore(cfg.CH, cfg.CHDatabase)
		if err != nil {
			return nil, err
		}
		h.store = store
		candidates := cfg.Candidates
		if candidates == nil {
			candidates = discovery.Engagement{Store: store, Tenant: h.tenant, RRFK: h.defaultRRFK}
		}
		h.discovery = discovery.Recommender{
			Candidates:   candidates,
			Store:        store,
			Tenant:       h.tenant,
			DefaultLimit: client.defaultLimit,
			RRFK:         h.defaultRRFK,
		}
	} else if cfg.Candidates != nil {
		return nil, fmt.Errorf("contentkit: Candidates requires the signal plane (CH)")
	}
	return h, nil
}

// Client returns the underlying content-plane client (advanced use).
func (h *EmbeddedHub) Client() *Client { return h.client }

// Tenant returns the pinned tenant value.
func (h *EmbeddedHub) Tenant() string { return h.tenant }

// Content returns a reference to a work of this hub's tenant.
func (h *EmbeddedHub) Content(contentKind, contentID string) ContentRef {
	return contentref.New(h.tenant, contentKind, contentID)
}

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

	// AffinityWeight boosts works the subject already engaged with by
	// (1 + AffinityWeight·last_score/100). 0 = off.
	AffinityWeight float32

	// DemoteSeen demotes already-seen / completed works.
	DemoteSeen bool
	// SeenPenalty multiplies seen-but-not-completed scores (default 0.85).
	SeenPenalty float32
	// CompletedPenalty multiplies completed scores (default 0.6).
	CompletedPenalty float32
	// DislikePenalty multiplies works the subject has net-negative explicit
	// feedback for (default 0.3). Always applied when view context is loaded
	// (i.e. AffinityWeight > 0 or DemoteSeen).
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

	// Candidate popularity per kind, at work level.
	idsByKind := map[string][]string{}
	for _, hit := range hits {
		idsByKind[hit.ContentKind] = append(idsByKind[hit.ContentKind], hit.ContentID)
	}
	popularity := map[ContentKey]float64{}
	for kind, ids := range idsByKind {
		scores, err := store.PopularityFor(ctx, h.tenant, kind, ids, p.PopularityWindow)
		if err != nil {
			return SearchResult{}, err
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
		K:       h.defaultRRFK,
		Weights: []float32{1, p.PopularityWeight},
	})

	// Per-subject view context: affinity boost + seen/completed demotion.
	var states map[ContentKey]signal.State
	if p.AffinityWeight > 0 || p.DemoteSeen {
		refs := make([]ContentRef, 0, len(hits))
		for _, hit := range hits {
			refs = append(refs, hit.Content())
		}
		states, err = store.States(ctx, h.tenant, p.Subject, refs)
		if err != nil {
			return SearchResult{}, err
		}
	}

	out := make([]SearchHit, 0, len(fused))
	for _, f := range fused {
		score := f.Score
		if st, ok := states[f.Ref().Content().Key()]; ok {
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

func (h *EmbeddedHub) Typeahead(ctx context.Context, userText string, opts TypeaheadOptions) ([]TypeaheadHit, error) {
	return h.client.Typeahead(ctx, userText, opts)
}

// RecHit is one ranked work of SimilarTo or Recommend.
type RecHit = discovery.Hit

// SimilarOptions controls SimilarTo (see discovery.SimilarOptions).
type SimilarOptions = discovery.SimilarOptions

// RecommendOptions controls Recommend (see discovery.RecommendOptions).
type RecommendOptions = discovery.RecommendOptions

// SimilarTo returns works like the anchor ("more like this") from the
// configured Candidates source (default: co-engagement).
func (h *EmbeddedHub) SimilarTo(ctx context.Context, ref ContentRef, opts SimilarOptions) ([]RecHit, error) {
	if h.store == nil {
		return nil, ErrSignalPlaneDisabled
	}
	return h.discovery.Similar(ctx, ref, opts)
}

// Recommend returns "for you" works for a subject from the configured
// Candidates source, excluding seen (unless IncludeSeen) and disliked works,
// with a popularity fill for cold start. The host hydrates the references.
func (h *EmbeddedHub) Recommend(ctx context.Context, subject signal.Subject, opts RecommendOptions) ([]RecHit, error) {
	if h.store == nil {
		return nil, ErrSignalPlaneDisabled
	}
	return h.discovery.Recommend(ctx, subject, opts)
}

// RefreshCoEngagement (re)materializes the content_pairs co-engagement rollup
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
// crash repair, and with Rebuild over a window after projection changes.
func (h *EmbeddedHub) RepairProjections(ctx context.Context, opts signal.RepairOptions) (signal.RepairResult, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.RepairResult{}, err
	}
	return store.RepairProjections(ctx, h.tenant, opts)
}

// Inventory reports canonical event volume per content kind and signal type.
func (h *EmbeddedHub) Inventory(ctx context.Context) ([]signal.InventoryRow, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Inventory(ctx, h.tenant)
}

// PurgeContentKinds irreversibly deletes whole content kinds from this tenant's
// signal plane (see signal.Store.PurgeContentKinds).
func (h *EmbeddedHub) PurgeContentKinds(ctx context.Context, contentKinds []string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.PurgeContentKinds(ctx, h.tenant, contentKinds)
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

// ForgetExposuresBefore clears only exposures from before the host's clear request.
func (h *EmbeddedHub) ForgetExposuresBefore(ctx context.Context, subject signal.Subject, before time.Time) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ForgetExposuresBefore(ctx, h.tenant, subject, before)
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

// RecordSignals applies each content kind's registered Scorer, then records
// the batch (see signal.Store.RecordSignals). A scorer error records nothing.
func (h *EmbeddedHub) RecordSignals(ctx context.Context, signals []signal.Signal) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	out := make([]signal.Signal, 0, len(signals))
	for _, s := range signals {
		if scorer, ok := h.scorers[s.ContentKind]; ok && scorer != nil {
			scored, err := scorer.Score(ctx, s)
			if err != nil {
				return fmt.Errorf("contentkit: scorer for %q: %w", s.ContentKind, err)
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

// Forget erases the subject's signals for one work and its versions
// (contentID set) or a whole content kind (contentID empty) — host "clear my
// history" support.
func (h *EmbeddedHub) Forget(ctx context.Context, subject signal.Subject, contentKind, contentID string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.Forget(ctx, h.tenant, subject, contentKind, contentID)
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

// SeenIDs returns the subject's seen-set for one content kind (the
// signal-plane half of the unseen anti-join). Use when the host wants to run
// its own diff against a custom-filtered universe instead of Unseen's
// registered catalog.
func (h *EmbeddedHub) SeenIDs(ctx context.Context, subject signal.Subject, contentKind string) (map[string]struct{}, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.SeenIDs(ctx, h.tenant, subject, contentKind)
}

// UnseenOptions controls Unseen reads.
type UnseenOptions struct {
	// ContentKind selects which catalog universe to diff against. Required.
	ContentKind string
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
	contentKind := strings.TrimSpace(opts.ContentKind)
	if contentKind == "" {
		return nil, fmt.Errorf("contentkit: UnseenOptions.ContentKind is required")
	}
	catalog, ok := h.catalogs[contentKind]
	if !ok || catalog == nil {
		return nil, fmt.Errorf("contentkit: no ContentCatalog registered for content kind %q", contentKind)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	universe, err := catalog.Universe(ctx, h.tenant, contentKind, CatalogQuery{Limit: opts.CatalogLimit})
	if err != nil {
		return nil, fmt.Errorf("contentkit: catalog universe for %q: %w", contentKind, err)
	}
	seen, err := store.SeenIDs(ctx, h.tenant, subject, contentKind)
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

func (h *EmbeddedHub) States(ctx context.Context, subject signal.Subject, refs []ContentRef) (map[ContentKey]signal.State, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.States(ctx, h.tenant, subject, refs)
}

func (h *EmbeddedHub) Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Popular(ctx, h.tenant, contentKind, opts)
}

// Metrics returns named window metrics (viewers, views, completions,
// feedback, ...) for the references (works or versions).
func (h *EmbeddedHub) Metrics(ctx context.Context, refs []ContentRef, window signal.Window) (map[ContentKey]signal.ContentMetrics, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Metrics(ctx, h.tenant, refs, window)
}

// PopularityFor scores a fixed candidate set (work ids of one kind) by the
// popularity ranking, returning content_id -> score. Use to rank a host-
// filtered universe (e.g. "galleries of artist X by popularity").
func (h *EmbeddedHub) PopularityFor(ctx context.Context, contentKind string, ids []string, window signal.Window) (map[string]float64, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.PopularityFor(ctx, h.tenant, contentKind, ids, window)
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
