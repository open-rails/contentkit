package signal

import "time"

// ActivityScope identifies the reader population of a ranking. An empty field
// includes every domain or language in the tenant's application.
type ActivityScope struct {
	Domain   string `json:"domain,omitempty"`
	Language string `json:"language,omitempty"`
}

// AccessContext is a server-observed, immutable access decision for a source
// event. Clients receive it in a signed receipt; they cannot assign these facts.
type AccessContext struct {
	ObservedAt       time.Time `json:"observed_at"`
	ContentAccess    string    `json:"content_access"`
	ViewerAccess     string    `json:"viewer_access"`
	PolicyRevision   string    `json:"policy_revision"`
	Audience         string    `json:"audience"`
	AudienceTiers    []string  `json:"audience_tiers,omitempty"`
	EligibleTiers    []string  `json:"eligible_tiers,omitempty"`
	EligibilityKnown bool      `json:"eligibility_known"`
	Domain           string    `json:"domain,omitempty"`
	Language         string    `json:"language,omitempty"`
}

// PopulationObservation records a reader's activity independently of whether
// they consume a particular work. Premium readers browsing free content remain
// part of the premium-eligible population.
type PopulationObservation struct {
	ID               string
	ContentKind      string
	Subject          Subject
	ObservedAt       time.Time
	Domain           string
	Language         string
	EligibleTiers    []string
	EligibilityKnown bool
}

// AccessInterval is one non-overlapping interval during which a work has this
// full-read audience. The catalog unions overlapping versions before returning
// intervals. Until is exclusive; an empty Until means still available.
type AccessInterval struct {
	ID            string
	ContentID     string
	From          time.Time
	Until         time.Time
	Audience      string
	AudienceTiers []string
}

// Availability is trusted catalog history within one common covered interval.
// Unknown legacy history must never be reconstructed from today's access rule.
type Availability struct {
	Intervals []AccessInterval
	From      time.Time
	Until     time.Time
	Known     bool
}

// NormalizationPriors smooth the quality observations within each access
// interval. They carry no reach weighting; PriorWeight controls that separately.
type NormalizationPriors struct {
	Engagement       float64
	EngagementWeight float64
	Completion       float64
	CompletionWeight float64
	Approval         float64
	ApprovalWeight   float64
	RevisitWeight    float64
}

// NormalizationOptions select event-time audience normalization. The named v2
// policy adds twenty pseudo-eligible nonreaders, a ranking heuristic rather than
// a confidence interval.
type NormalizationOptions struct {
	Availability Availability
	Scope        ActivityScope
	PriorWeight  float64
	Quality      NormalizationPriors
}

// NormalizationMetrics preserve the raw participation rate and the separate
// quality contributions. Each quality term already includes the segment's
// smoothed participation rate and duration weight, so differently sized free
// and premium audiences cannot drown one another when a work changes access.
type NormalizationMetrics struct {
	RawRate    float64 `json:"raw_rate"`
	Rate       float64 `json:"rate"`
	Engagement float64 `json:"engagement"`
	Completion float64 `json:"completion"`
	Approval   float64 `json:"approval"`
	Revisit    float64 `json:"revisit"`
}
