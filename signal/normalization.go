package signal

import (
	"time"
)

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
