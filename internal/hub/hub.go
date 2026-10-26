package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
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

	// Content-plane defaults (as in ClientConfig).
	DefaultLanguage string
	DefaultLimit    int
	// DefaultRRFK controls deterministic popularity fusion in personalized search (default 60).
	DefaultRRFK int

	// Signal plane (ClickHouse). Optional: omit CH to run content-only
	// (signal methods return ErrSignalPlaneDisabled). CHDatabase
	// is the hub's dedicated ClickHouse database; apply
	// migrations.ClickHouse and gate startup on signal.CheckSchema.
	CH         signal.Conn
	CHDatabase string
	// SignalPlane: a signal plane is configured that SetSignals opens later;
	// until then signal methods return ErrSignalPlaneDown.
	SignalPlane bool

	// Tenant is the single tenant of this embedded hub. Required: every
	// document, signal, cursor and result carries it.
	Tenant string
}

// Hub is one tenant's search client and signal plane: the engine's internal
// surface the discovery routes build on.
type Hub struct {
	defaultRRFK int
	client      *Client
	store       atomic.Pointer[signal.Store]
	configured  bool
	tenant      string
}

// New builds the hub of one tenant.
func New(cfg Config) (*Hub, error) {
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
	h := &Hub{
		client:      client,
		defaultRRFK: cfg.DefaultRRFK,
		tenant:      client.Tenant(),
		configured:  cfg.CH != nil || cfg.SignalPlane,
	}
	if h.defaultRRFK <= 0 {
		h.defaultRRFK = 60
	}
	if cfg.CH != nil {
		store, err := signal.NewStore(cfg.CH, cfg.CHDatabase)
		if err != nil {
			return nil, err
		}
		h.store.Store(store)
	}
	return h, nil
}

// Client returns the underlying content-plane client (advanced use).
func (h *Hub) Client() *Client { return h.client }

// Tenant returns the pinned tenant value.
func (h *Hub) Tenant() string { return h.tenant }

// Content returns a reference to a work of this hub's tenant.
func (h *Hub) Content(contentKind, contentID string) ContentRef {
	return contentref.New(h.tenant, contentKind, contentID)
}

func (h *Hub) requireStore() (*signal.Store, error) {
	if s := h.store.Load(); s != nil {
		return s, nil
	}
	if h.configured {
		return nil, ErrSignalPlaneDown
	}
	return nil, ErrSignalPlaneDisabled
}

// --- Content plane ---

// Personalization fuses signal aggregates into search ranking. Recall is
// unchanged: a ranking-only layer over the candidate set, which discovery
// applies for signed-in readers.
type Personalization struct {
	Subject signal.Subject

	// PopularityWeight is the RRF weight of the candidate-set popularity
	// list blended with the content ranking. Defaults to 0.25.
	PopularityWeight float32
	// Popularity scores candidate works of one kind, higher first; works
	// without a score are absent. Nil leaves popularity out.
	Popularity func(ctx context.Context, kind string, ids []string) (map[string]float64, error)

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

// KeywordMatch is the SQL selecting every content item matching userText; see
// Client.KeywordMatch.
func (h *Hub) KeywordMatch(ctx context.Context, userText string, opts SearchOptions) (search.Match, error) {
	return h.client.KeywordMatch(ctx, userText, opts)
}

func (h *Hub) Search(ctx context.Context, userText string, opts HubSearchOptions) (SearchResult, error) {
	if opts.Personalize == nil {
		return h.client.Search(ctx, userText, opts.SearchOptions)
	}
	store, err := h.requireStore()
	if err != nil {
		return h.client.Search(ctx, userText, opts.SearchOptions) // no signal plane: unpersonalized
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

	groups, truncated, err := h.client.searchGroups(ctx, userText, opts.SearchOptions, nil)
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
			return unpersonalized(ctx, content, offset, limit, err), nil
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
				if p.AffinityWeight > 0 && st.Quality > 0 {
					score *= 1 + p.AffinityWeight*float32(st.Quality)
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
func (h *Hub) Signals() *signal.Store { return h.store.Load() }

// SetSignals opens the configured signal plane over s, or takes it down
// (nil) while ClickHouse is unreachable.
func (h *Hub) SetSignals(s *signal.Store) { h.store.Store(s) }

// SignalsConfigured reports whether the hub has a signal plane, up or down.
func (h *Hub) SignalsConfigured() bool { return h.configured }

// RepairProjections is the bounded, host-scheduled projection repair (see
// signal.Store.RepairProjections): run it periodically with IngestedSince for
// crash repair, and with Rebuild over a window after projection changes.
func (h *Hub) RepairProjections(ctx context.Context, opts signal.RepairOptions) (signal.RepairResult, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.RepairResult{}, err
	}
	return store.RepairProjections(ctx, h.tenant, opts)
}

// Inventory reports canonical event volume per content kind and signal type.
func (h *Hub) Inventory(ctx context.Context) ([]signal.InventoryRow, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Inventory(ctx, h.tenant)
}

// PurgeContentKinds irreversibly deletes whole content kinds from this tenant's
// signal plane (see signal.Store.PurgeContentKinds).
func (h *Hub) PurgeContentKinds(ctx context.Context, contentKinds []string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.PurgeContentKinds(ctx, h.tenant, contentKinds)
}

// RecordExposures logs one row per result list and stage (served, rendered,
// visible) so clicks can be attributed to what was actually exposed. Hosts call
// it once per list per stage, never per item.
func (h *Hub) RecordExposures(ctx context.Context, exposures []signal.Exposure) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.RecordExposures(ctx, h.tenant, exposures)
}

// ForgetExposures clears a subject's result-list exposures (search history).
func (h *Hub) ForgetExposures(ctx context.Context, subject signal.Subject) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ForgetExposures(ctx, h.tenant, subject)
}

// ForgetExposuresBefore clears only exposures from before the host's clear request.
func (h *Hub) ForgetExposuresBefore(ctx context.Context, subject signal.Subject, before time.Time) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ForgetExposuresBefore(ctx, h.tenant, subject, before)
}

// Attribution exports renders at one stage with their clicks joined (see
// signal.Store.Attribution); the evaluation dataset source.
func (h *Hub) Attribution(ctx context.Context, opts signal.AttributionOptions) (signal.AttributionPage, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.AttributionPage{}, err
	}
	return store.Attribution(ctx, h.tenant, opts)
}

// --- Signal plane ---

// RecordSignals records the batch (see signal.Store.RecordSignals).
func (h *Hub) RecordSignals(ctx context.Context, signals []signal.Signal) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.RecordSignals(ctx, h.tenant, signals)
}

// PopulationCoverage is the earliest trusted eligible-audience observation in
// this app/kind/domain/language and requested activity window.
func (h *Hub) PopulationCoverage(ctx context.Context, kind string, scope signal.ActivityScope, window signal.Window) (time.Time, bool, error) {
	store, err := h.requireStore()
	if err != nil {
		return time.Time{}, false, err
	}
	return store.PopulationCoverage(ctx, h.tenant, kind, scope, window)
}

// ReadContent returns the contents of kind read in the window.
func (h *Hub) ReadContent(ctx context.Context, kind string, window signal.Window) ([]string, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.ReadContent(ctx, h.tenant, kind, window)
}

// --- Discovery plane ---

func (h *Hub) History(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) ([]signal.StateRow, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.History(ctx, h.tenant, subject, opts)
}

// Forget erases the subject's signals for one work and its versions
// (contentID set) or a whole content kind (contentID empty) — host "clear my
// history" support.
func (h *Hub) Forget(ctx context.Context, subject signal.Subject, contentKind, contentID string) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.Forget(ctx, h.tenant, subject, contentKind, contentID)
}

// EraseSubjects permanently erases subjects from this tenant's signal plane:
// see signal.Store.EraseSubjects for the completion contract. Shared accounts
// exist in several tenants: each host erases its own tenant.
func (h *Hub) EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.ErasureReport{}, err
	}
	return store.EraseSubjects(ctx, []string{h.tenant}, subjects)
}

// AnonymizeSubject preserves anonymous engagement while unlinking an account.
func (h *Hub) AnonymizeSubject(ctx context.Context, old, anonymous signal.Subject) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.AnonymizeSubject(ctx, h.tenant, old, anonymous)
}

// EnforceErasures physically removes residue of every recorded erasure of
// this tenant (see signal.Store.EnforceErasures). The host invokes it explicitly;
// ContentKit does not schedule an erasure sweep.
func (h *Hub) EnforceErasures(ctx context.Context) (signal.ErasureReport, error) {
	store, err := h.requireStore()
	if err != nil {
		return signal.ErasureReport{}, err
	}
	return store.EnforceErasures(ctx, h.tenant)
}

// HistoryCount returns the total row count History would paginate over.
func (h *Hub) HistoryCount(ctx context.Context, subject signal.Subject, opts signal.HistoryOptions) (int64, error) {
	store, err := h.requireStore()
	if err != nil {
		return 0, err
	}
	return store.HistoryCount(ctx, h.tenant, subject, opts)
}

// SeenIDs returns the subject's seen-set for one content kind (the
// signal-plane half of an unseen anti-join).
func (h *Hub) SeenIDs(ctx context.Context, subject signal.Subject, contentKind string) (map[string]struct{}, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.SeenIDs(ctx, h.tenant, subject, contentKind)
}

func (h *Hub) States(ctx context.Context, subject signal.Subject, refs []ContentRef) (map[ContentKey]signal.State, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.States(ctx, h.tenant, subject, refs)
}

// ForgetContents erases purged content's signals for every subject.
func (h *Hub) ForgetContents(ctx context.Context, contents []signal.ForgottenContent) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ForgetContents(ctx, h.tenant, contents)
}

// Activity is a content kind's distinct measured readership per day of window.
func (h *Hub) Activity(ctx context.Context, contentKind string, window signal.Window, cohorts ...signal.ActivityCohort) ([]signal.DayActivity, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Activity(ctx, h.tenant, contentKind, window, cohorts...)
}

func (h *Hub) Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Popular(ctx, h.tenant, contentKind, opts)
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

func (h *Hub) CompletedCount(ctx context.Context, subject signal.Subject, kind string) (int64, error) {
	store, err := h.requireStore()
	if err != nil {
		return 0, err
	}
	return store.CompletedCount(ctx, h.tenant, subject, kind)
}

func (h *Hub) ImportLegacyReading(ctx context.Context, in []signal.LegacyReading) error {
	store, err := h.requireStore()
	if err != nil {
		return err
	}
	return store.ImportLegacyReading(ctx, h.tenant, in)
}
