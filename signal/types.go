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

const (
	SubjectKindUser = "user"
)

const (
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

// Signal is one logical source event: a consumption session, reaction, click,
// rating. Identity is (tenant, content ref, subject, Type, EventID):
// re-delivering the same identity never adds another event, whatever its
// arrival order, batch or merge state. Never emit one event per scroll/frame
// tick. A version-scoped reference records that version's consumption; the
// work-level reference records the work once across versions.
type Signal struct {
	ContentRef
	Subject Subject

	// Type is host-defined; route-open/view/preview signals are rejected.
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

	// Value is explicit feedback (+1 like, -1 dislike, rating).
	Value float64
	// Resume is an opaque host pointer for "pick up where you left off".
	Resume string

	// Payload holds bounded context, JSON-encoded at rest.
	Payload map[string]any

	// Access is a trusted access snapshot at the event's observation time. Nil
	// marks legacy events whose audience cannot be inferred retroactively.
	Access *AccessContext

	// Measurement is trusted measured consumption. It is stored separately
	// from generic feedback and never increments a route-open counter.
	Measurement *Measurement
}

const (
	SurfaceSearch = "search"
)

const (
	SurfaceForYou = "foryou"
)

const (
	SurfaceSimilar = "similar"
)

const (
	SurfacePopular = "popular"
)

const (
	SurfaceOrganic = "organic"
)

// ExposureStage says how far a result list got: served by the API, rendered
// by the client, or actually visible to the subject. A click is attributed
// against the stage the evaluation asks for; an item absent from that stage's
// list was not exposed there and is never a negative example.
type ExposureStage string

const (
	StageServed ExposureStage = "served"
)

const (
	StageRendered ExposureStage = "rendered"
)

const (
	StageVisible ExposureStage = "visible"
)

// Placement is one shown content reference and its absolute 1-based position
// in the render.
type Placement struct {
	ContentRef
	Position uint32
	// Access preserves the trusted eligibility decision made when rendered.
	Access *AccessContext
}

// Exposure is one result list at one stage: the cumulative set of placements
// the stage reached, as one row. Identity is (tenant, RenderID, Stage);
// re-sending replaces, a higher Revision supersedes (a visible list that grows
// as the subject scrolls). Never one row per item or per scroll tick. QueryID
// groups the pages/renders of one query and keeps its normalized search text.
type Exposure struct {
	RenderID    string // required, stable per render, shared with its clicks
	Stage       ExposureStage
	Revision    uint64
	QueryID     string
	SearchQuery string // normalized search text, retained with analytics
	Surface     string
	Ranker      string // ranking configuration identity for offline comparison
	Language    string
	Subject     Subject // optional for anonymous renders
	Shown       []Placement
	OccurredAt  time.Time // required
}

const (
	PayloadKeyRenderID = "render_id"
)

const (
	PayloadKeySurface = "surface"
)

const (
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
	Seen         bool
	FirstSeenAt  time.Time
	LastSignalAt time.Time
	LastReadAt   time.Time
	ActiveMS     uint64
	MaxProgress  uint32
	ProgressMax  uint32
	Completed    bool
	Resume       string
	Quality      float64 // bounded per-person measured coverage/attention quality; legacy resume carries zero
	NetValue     float64
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
)

const (
	// HistorySeen returns items with MaxProgress > 0.
	HistorySeen HistoryStatus = "seen"
)

const (
	// HistoryInProgress returns seen-but-not-completed items.
	HistoryInProgress HistoryStatus = "in_progress"
)

const (
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

// Window is a literal range of whole UTC calendar days, [From, To). A zero
// From or To leaves that side unbounded; the zero Window is all time. Every
// qualifying event inside the window counts with equal weight: there is no age
// decay, and an event's contribution does not depend on where in the window it
// falls.
type Window struct {
	From time.Time // inclusive UTC midnight
	To   time.Time // exclusive UTC midnight
}

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

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// PopularHit is one ranked work from prepared daily participation. Only works
// with at least one qualified reader in the window rank; editions never rank.
type PopularHit struct {
	ContentRef
	Score float64
}

// CoEngagedOptions controls co-engagement queries ("subjects who engaged with
// X also engaged with Y"). Co-engagement is work-level.
type CoEngagedOptions struct {
	// ContentKinds limits result kinds. Empty = all.
	ContentKinds []string
	// Window bounds the event scan. Zero = all time.
	Window Window
	// MaxSubjects caps the anchor's qualified readers (defaults to 10000).
	// Readers are selected by a stable identity hash, with identity tie-breakers.
	MaxSubjects int
	Limit       int // default 50
}

// CoEngagedHit is one co-engaged work. Strength is the NET co-engaged subject
// count: subjects with non-negative engagement count for, subjects with
// negative explicit feedback count against.
type CoEngagedHit struct {
	ContentRef
	Strength int64
}

// TopStatesOptions controls TopStates (recommendation seeds).
type TopStatesOptions struct {
	ContentKinds []string
	// ExcludeNegative drops content the subject has net-negative explicit
	// feedback for (a disliked item must not seed recommendations).
	ExcludeNegative bool
	Limit           int // default 10
}
