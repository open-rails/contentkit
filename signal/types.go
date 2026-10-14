// Package signal implements ContentKit's signal plane: an append-only stream of
// host-defined interaction signals plus a durable per-(subject, content)
// current-state projection, both stored in ClickHouse.
//
// The signal plane records *what each subject did and thought about each
// content reference* and projects that into fast reads for history, unseen,
// engagement, popularity, and (above this package) personalized search and
// recommendations.
//
// Mechanism vs meaning: this package owns storage, aggregation, and queries.
// Signal types, content kinds, scoring weights, and completion rules are
// host-defined data — no business noun appears in the schema. Every key,
// index and cursor carries the tenant.
package signal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// ContentRef is the tenant-scoped content reference every signal carries.
type ContentRef = contentref.ContentRef

// ContentKey is the comparable form of a ContentRef (map keys).
type ContentKey = contentref.ContentKey

// Subject is who acted: a resolved user id (e.g. from authkit) or, for
// anonymous traffic, a session-key hash. Exactly one of the two must be set.
//
// History/unseen are meaningful only for logged-in subjects; anonymous signals
// still feed popularity/engagement aggregates.
type Subject struct {
	UserID  string
	AnonKey string
}

// SubjectKind values stored in the subject_kind column.
const (
	SubjectKindUser = "user"
	SubjectKindAnon = "anon"
)

// Kind returns "user" or "anon".
func (s Subject) Kind() string {
	if strings.TrimSpace(s.UserID) != "" {
		return SubjectKindUser
	}
	return SubjectKindAnon
}

// Key returns the stored subject identifier.
func (s Subject) Key() string {
	if strings.TrimSpace(s.UserID) != "" {
		return strings.TrimSpace(s.UserID)
	}
	return strings.TrimSpace(s.AnonKey)
}

// Validate checks that exactly one of UserID / AnonKey is set.
func (s Subject) Validate() error {
	user := strings.TrimSpace(s.UserID) != ""
	anon := strings.TrimSpace(s.AnonKey) != ""
	if user == anon { // neither or both
		return fmt.Errorf("signal: exactly one of Subject.UserID / Subject.AnonKey must be set")
	}
	return nil
}

// TypeView is the consumption signal type. Viewer counts, popularity and
// progress/completion/resume state read only view events; clicks and feedback
// stay separate signals.
const TypeView = "view"

// Signal is one logical source event: a consumption session, reaction, click,
// rating. Identity is (tenant, content ref, subject, Type, EventID):
// re-delivering the same identity never adds another event, whatever its
// arrival order, batch or merge state. Never emit one event per scroll/frame
// tick. A version-scoped reference records that version's consumption; the
// work-level reference records the work once across versions.
type Signal struct {
	ContentRef
	Subject Subject

	// Type is host-defined except TypeView.
	Type string

	// EventID is the required stable source identity. Retries must reuse it;
	// never mint a new id (or time) per delivery attempt.
	EventID string

	// Revision orders cumulative snapshots of one EventID (for example a
	// checkpointed consumption session, or a subject's current preference):
	// the highest revision is the event, lower ones are superseded. Leave 0 for
	// immutable events. Conflicting content at an equal revision resolves by a
	// deterministic content hash, not by arrival order.
	Revision uint64

	// OccurredAt is the required immutable source time (a session's start). It
	// selects the UTC day the event counts in.
	OccurredAt time.Time

	// Consumption measurements, cumulative for the revision.
	DurationS   uint32 // active time
	Progress    uint32 // numerator (pages / scroll % / watched s)
	ProgressMax uint32 // denominator (page count / 100 / duration)

	// Value is explicit feedback (+1 like, -1 dislike, rating). State and
	// windows sum the canonical values.
	Value float64

	// Score is the engagement score; a registered Scorer fills it together with
	// Progress/ProgressMax/Completed.
	Score int16

	// Completed per the content kind's completion rule.
	Completed bool

	// Resume is an opaque host pointer for "pick up where you left off".
	Resume string

	// Payload holds bounded context, JSON-encoded at rest.
	Payload map[string]any
}

func (s Signal) validate(tenant string) error {
	if err := checkRef(tenant, s.ContentRef); err != nil {
		return err
	}
	if err := s.Subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Type) == "" {
		return fmt.Errorf("signal: Type is required")
	}
	if strings.TrimSpace(s.EventID) == "" {
		return fmt.Errorf("signal: EventID is required (stable source identity reused by retries)")
	}
	if s.OccurredAt.IsZero() {
		return fmt.Errorf("signal: OccurredAt is required (immutable source time)")
	}
	if err := checkIdentifiers("Subject", s.Subject.Key(), "Type", s.Type, "EventID", s.EventID); err != nil {
		return err
	}
	return checkLen("Resume", s.Resume, MaxResumeBytes)
}

// checkRef validates a reference and pins it to the store call's tenant: a
// reference of another tenant is an error, never silently rewritten.
func checkRef(tenant string, ref ContentRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("signal: %w", err)
	}
	if ref.TenantID != tenant {
		return fmt.Errorf("signal: %s belongs to tenant %q, not %q", ref, ref.TenantID, tenant)
	}
	return checkIdentifiers("ContentKind", ref.ContentKind, "ContentID", ref.ContentID, "ContentVersionID", ref.Version())
}

// Scored is the result of a content kind's Scorer.
type Scored struct {
	Score       int16
	Progress    uint32
	ProgressMax uint32
	Completed   bool
}

// Scorer maps a raw signal (the "session") to a normalized engagement score,
// generic progress, and whether it counts as "completed". Host-provided per
// content kind; this is where kinds differ while the hub stays generic.
//
// Examples: gallery — progress = max page reached / page count, completed at
// ≥90%; blog post — read-time + scroll depth; video — watch %.
type Scorer interface {
	Score(ctx context.Context, s Signal) (Scored, error)
}

// ScorerFunc adapts a function to the Scorer interface.
type ScorerFunc func(ctx context.Context, s Signal) (Scored, error)

func (f ScorerFunc) Score(ctx context.Context, s Signal) (Scored, error) { return f(ctx, s) }

// Render surfaces. Hosts may use other values; these are the common ones
// evaluation distinguishes.
const (
	SurfaceSearch  = "search"
	SurfaceForYou  = "foryou"
	SurfaceSimilar = "similar"
	SurfacePopular = "popular"
	SurfaceOrganic = "organic"
)

// ExposureStage says how far a result list got: served by the API, rendered
// by the client, or actually visible to the subject. A click is attributed
// against the stage the evaluation asks for; an item absent from that stage's
// list was not exposed there and is never a negative example.
type ExposureStage string

const (
	StageServed   ExposureStage = "served"
	StageRendered ExposureStage = "rendered"
	StageVisible  ExposureStage = "visible"
)

func (s ExposureStage) validate() error {
	switch s {
	case StageServed, StageRendered, StageVisible:
		return nil
	}
	return fmt.Errorf("exposure: invalid stage %q", s)
}

// Placement is one shown content reference and its absolute 1-based position
// in the render.
type Placement struct {
	ContentRef
	Position uint32
}

// Exposure is one result list at one stage: the cumulative set of placements
// the stage reached, as one row. Identity is (tenant, RenderID, Stage);
// re-sending replaces, a higher Revision supersedes (a visible list that grows
// as the subject scrolls). Never one row per item or per scroll tick. No query
// text is stored; QueryID groups the pages/renders of one query.
type Exposure struct {
	RenderID   string // required, stable per render, shared with its clicks
	Stage      ExposureStage
	Revision   uint64
	QueryID    string
	Surface    string
	Ranker     string // ranking configuration identity for offline comparison
	Language   string
	Subject    Subject // optional for anonymous renders
	Shown      []Placement
	OccurredAt time.Time // required
}

func (e Exposure) validate(tenant string) error {
	if strings.TrimSpace(e.RenderID) == "" {
		return fmt.Errorf("exposure: RenderID is required")
	}
	if err := e.Stage.validate(); err != nil {
		return err
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("exposure: OccurredAt is required")
	}
	if len(e.Shown) == 0 {
		return fmt.Errorf("exposure: at least one placement is required")
	}
	if len(e.Shown) > MaxShownPerExposure {
		return &LimitError{Field: "Shown", Limit: MaxShownPerExposure, Got: len(e.Shown)}
	}
	seen := map[uint32]struct{}{}
	for i, p := range e.Shown {
		if err := checkRef(tenant, p.ContentRef); err != nil {
			return fmt.Errorf("exposure: placement %d: %w", i, err)
		}
		if p.Position == 0 {
			return fmt.Errorf("exposure: placement %d: Position is required (1-based)", i)
		}
		if _, dup := seen[p.Position]; dup {
			return fmt.Errorf("exposure: duplicate position %d", p.Position)
		}
		seen[p.Position] = struct{}{}
	}
	if e.Subject != (Subject{}) {
		if err := e.Subject.Validate(); err != nil {
			return err
		}
	}
	return checkIdentifiers("RenderID", e.RenderID, "QueryID", e.QueryID, "Surface", e.Surface,
		"Ranker", e.Ranker, "Language", e.Language, "Subject", e.Subject.Key())
}

// Standardized click-attribution payload keys, set via WithAttribution so
// evaluation can join clicks to exposures on the render id.
const (
	PayloadKeyRenderID = "render_id"
	PayloadKeySurface  = "surface"
	PayloadKeyPosition = "position"
)

// Attribution links a click/engagement signal to the render that produced it.
type Attribution struct {
	RenderID string // the Exposure.RenderID the click came from
	Surface  string
	Position uint32 // 1-based position of the clicked item within that render
}

// WithAttribution returns a copy of the signal with attribution written into a
// fresh Payload under the standardized keys (existing payload entries are
// preserved). Zero-valued fields are omitted.
func (s Signal) WithAttribution(a Attribution) Signal {
	payload := make(map[string]any, len(s.Payload)+3)
	for k, v := range s.Payload {
		payload[k] = v
	}
	if strings.TrimSpace(a.RenderID) != "" {
		payload[PayloadKeyRenderID] = a.RenderID
	}
	if strings.TrimSpace(a.Surface) != "" {
		payload[PayloadKeySurface] = a.Surface
	}
	if a.Position != 0 {
		payload[PayloadKeyPosition] = a.Position
	}
	s.Payload = payload
	return s
}

// Attribution reads attribution back from the signal's payload, tolerant of the
// numeric type a JSON round-trip produces. Missing keys yield zero values.
func (s Signal) Attribution() Attribution {
	a := Attribution{}
	if s.Payload == nil {
		return a
	}
	if v, ok := s.Payload[PayloadKeyRenderID].(string); ok {
		a.RenderID = v
	}
	if v, ok := s.Payload[PayloadKeySurface].(string); ok {
		a.Surface = v
	}
	a.Position = payloadUint32(s.Payload[PayloadKeyPosition])
	return a
}

// payloadUint32 coerces the numeric types a payload value may carry (native or
// JSON-decoded) into a uint32; negatives and non-numbers yield 0.
func payloadUint32(v any) uint32 {
	switch n := v.(type) {
	case uint32:
		return n
	case int:
		if n >= 0 {
			return uint32(n)
		}
	case int64:
		if n >= 0 {
			return uint32(n)
		}
	case float64:
		if n >= 0 {
			return uint32(n)
		}
	}
	return 0
}

// State is one subject's compact, indefinitely retained standing with one
// content reference, derived from its canonical events.
type State struct {
	Seen         bool // MaxProgress > 0
	FirstSeenAt  time.Time
	LastSignalAt time.Time
	LastViewAt   time.Time
	TotalEvents  uint32 // canonical events of every type
	Views        uint32 // canonical view events (sessions)
	Completions  uint32 // completed views
	ActiveS      uint64 // summed view DurationS
	MaxProgress  uint32 // progress bar = MaxProgress / ProgressMax
	ProgressMax  uint32
	Completed    bool
	Resume       string // latest non-empty resume pointer
	LastScore    int16  // score of the latest view
	// NetValue sums canonical feedback Values. Negative = current sentiment is
	// negative; recommendations exclude such content.
	NetValue float64
	Feedback uint32 // canonical events with a non-zero Value
}

// StateRow is a State with its content reference, as returned by History.
type StateRow struct {
	ContentRef
	State
}

// HistoryStatus filters History results.
type HistoryStatus string

const (
	// HistoryAny returns every content item the subject has any signal for.
	HistoryAny HistoryStatus = ""
	// HistorySeen returns items with MaxProgress > 0.
	HistorySeen HistoryStatus = "seen"
	// HistoryInProgress returns seen-but-not-completed items.
	HistoryInProgress HistoryStatus = "in_progress"
	// HistoryCompleted returns completed items.
	HistoryCompleted HistoryStatus = "completed"
)

// HistoryOptions controls History reads. History is work-level: version rows
// are read by States with explicit references.
type HistoryOptions struct {
	// ContentKind limits results to one kind. Empty = all kinds.
	ContentKind string
	Status      HistoryStatus
	// Since drops rows whose relevant activity is older (e.g. host "clear
	// history before X" features). Seen statuses use the last view; HistoryAny
	// uses the last signal. Zero = no lower bound.
	Since  time.Time
	Limit  int // default 50
	Offset int
}

// ContentMetrics are named, separately defined statistics for one content
// reference over a window, computed from canonical events. Each subject counts
// once per metric that says "subjects"; sessions and feedback are never
// relabeled as views.
type ContentMetrics struct {
	Viewers     uint64 // subjects with at least one view
	UserViewers uint64
	AnonViewers uint64
	Views       uint64 // view events (sessions)
	Completions uint64 // completed views
	Completers  uint64 // subjects with a completed view
	ActiveS     uint64 // summed view DurationS
	ScoreSum    int64  // summed view scores; ScoreSum/Views is the mean
	// ViewerEngagementSum sums each viewer's mean session score, normalized
	// and clamped to [0,1]. Every viewer contributes at most one unit.
	ViewerEngagementSum float64
	ReturningViewers    uint64 // subjects with more than one canonical view in the window
	Events              uint64 // canonical events of every type
	ValueSum            float64
	// PositiveSubjects / NegativeSubjects: subjects whose summed feedback
	// Value in the window is > 0 / < 0.
	PositiveSubjects uint64
	NegativeSubjects uint64
	SignalCounts     map[string]uint64 // canonical events per type
}

// Window is a literal range of whole UTC calendar days, [From, To). A zero
// From or To leaves that side unbounded; the zero Window is all time. Every
// qualifying event inside the window counts with equal weight: there is no age
// decay, and an event's contribution does not depend on where in the window it
// falls.
type Window struct {
	From time.Time // inclusive UTC midnight
	To   time.Time // exclusive UTC midnight
}

// LastDays returns the n most recent UTC calendar days including the day that
// contains now: [midnight(now) - (n-1) days, midnight(now) + 1 day). The window
// moves only at UTC midnight, so String is a stable cache key within a day.
// n must be positive (Validate reports otherwise).
func LastDays(n int, now time.Time) Window {
	today := utcDay(now)
	return Window{From: today.AddDate(0, 0, 1-n), To: today.AddDate(0, 0, 1)}
}

// Between returns the whole UTC days [from, to); both must be UTC midnights.
func Between(from, to time.Time) Window { return Window{From: from, To: to} }

// AllTime returns the unbounded window.
func AllTime() Window { return Window{} }

// Validate reports a window that is not a non-empty range of whole UTC days.
func (w Window) Validate() error {
	for _, t := range []time.Time{w.From, w.To} {
		if !t.IsZero() && !t.Equal(utcDay(t)) {
			return fmt.Errorf("signal: window bound %s is not a UTC midnight", t.Format(time.RFC3339Nano))
		}
	}
	if !w.From.IsZero() && !w.To.IsZero() && !w.From.Before(w.To) {
		return fmt.Errorf("signal: empty window [%s, %s)", w.From.UTC().Format(time.DateOnly), w.To.UTC().Format(time.DateOnly))
	}
	return nil
}

// String renders the window as "[from,to)" dates ("*" when unbounded).
func (w Window) String() string {
	day := func(t time.Time) string {
		if t.IsZero() {
			return "*"
		}
		return t.UTC().Format(time.DateOnly)
	}
	return "[" + day(w.From) + "," + day(w.To) + ")"
}

// predicate renders " AND column >= ? AND column < ?" for the bounded sides.
func (w Window) predicate(column string) (string, []any) {
	var (
		sb   strings.Builder
		args []any
	)
	if !w.From.IsZero() {
		sb.WriteString(" AND " + column + " >= ?")
		args = append(args, w.From.UTC())
	}
	if !w.To.IsZero() {
		sb.WriteString(" AND " + column + " < ?")
		args = append(args, w.To.UTC())
	}
	return sb.String(), args
}

// dayPredicate renders the window over a Date column.
func (w Window) dayPredicate(column string) (string, []any) {
	var (
		sb   strings.Builder
		args []any
	)
	if !w.From.IsZero() {
		sb.WriteString(" AND " + column + " >= toDate(?)")
		args = append(args, w.From.UTC().Format(time.DateOnly))
	}
	if !w.To.IsZero() {
		sb.WriteString(" AND " + column + " < toDate(?)")
		args = append(args, w.To.UTC().Format(time.DateOnly))
	}
	return sb.String(), args
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// RankWeights tunes the default popularity ranking:
//
//	volume  = log10(1 + viewers)
//	quality = (score_sum + PriorScore·PriorWeight) / (views + PriorWeight)
//	rank    = volume × max(QualityFloor, quality)
//
// Qualifying views have equal time weight inside the selected window. A score
// of zero is a valid observation and participates in the mean.
type RankWeights struct {
	// PriorWeight is the strength of the Bayesian prior in pseudo-views.
	// Defaults to 10.
	PriorWeight float64
	// PriorScore is the prior mean engagement score. Defaults to 0.
	PriorScore float64
	// QualityFloor keeps pure-volume ranking meaningful when hosts record no
	// scores. Defaults to 1.
	QualityFloor float64
}

func (w RankWeights) withDefaults() RankWeights {
	if w.PriorWeight <= 0 {
		w.PriorWeight = 10
	}
	if w.QualityFloor <= 0 {
		w.QualityFloor = 1
	}
	return w
}

// PopularOptions controls Popular reads.
type PopularOptions struct {
	Window Window
	Limit  int // default 20

	// Weights tunes the default ranking formula.
	Weights RankWeights

	// RankExpr, when set, REPLACES the default ranking with a host-supplied
	// ClickHouse expression (trusted SQL). It may reference the window metric
	// columns: viewers, user_viewers, anon_viewers, views, completions,
	// completers, active_s, score_sum, viewer_engagement_sum, returning_viewers,
	// events, value_sum, positive_subjects,
	// negative_subjects.
	RankExpr string
}

// PopularHit is one ranked work from Popular. Only works with at least one
// view in the window rank; version rows never rank.
type PopularHit struct {
	ContentRef
	ContentMetrics
	Score float64
}

// CoEngagedOptions controls co-engagement queries ("subjects who engaged with
// X also engaged with Y"). Co-engagement is work-level.
type CoEngagedOptions struct {
	// ContentKinds limits result kinds. Empty = all.
	ContentKinds []string
	// Window bounds the event scan. Zero = all time.
	Window Window
	// MaxSubjects caps the engaged-subject set read from the anchor
	// (defaults to 10000).
	MaxSubjects int
	// SkipRollup forces the query-time event scan even when the content_pairs
	// rollup has rows (e.g. for freshness-critical reads).
	SkipRollup bool
	Limit      int // default 50
}

// CoEngagedHit is one co-engaged work. Strength is the NET co-engaged subject
// count: subjects with non-negative engagement count for, subjects with
// negative explicit feedback count against.
type CoEngagedHit struct {
	ContentRef
	Strength int64
}

// RefreshCoEngagementOptions controls RefreshCoEngagement.
type RefreshCoEngagementOptions struct {
	// Window bounds which events feed the rollup (zero = all time).
	Window Window
	// MaxContentPerSubject caps each subject's contribution to pair
	// generation (defaults to 100), bounding the cross-product.
	MaxContentPerSubject int
}

// TopStatesOptions controls TopStates (recommendation seeds).
type TopStatesOptions struct {
	ContentKinds []string
	// ExcludeNegative drops content the subject has net-negative explicit
	// feedback for (a disliked item must not seed recommendations).
	ExcludeNegative bool
	Limit           int // default 10
}
