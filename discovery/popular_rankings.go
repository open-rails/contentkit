package discovery

import (
	"time"
)

// PopularitySnapshot describes the prepared ranking actually served. A pending
// bucket has Ready=false; a successfully prepared empty ranking has Ready=true.
type PopularitySnapshot struct {
	Bucket string `json:"bucket"`
	Ready  bool   `json:"ready"`
	// WindowFrom/WindowUntil are the resolved bucket requested at preparation.
	WindowFrom  *time.Time `json:"window_from,omitempty"`
	WindowUntil *time.Time `json:"window_until,omitempty"`
	// From/Until are the covered whole UTC days actually ranked.
	From          *time.Time `json:"from,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
	AvailableFrom *time.Time `json:"available_from,omitempty"`
	ComputedAt    *time.Time `json:"computed_at,omitempty"`
	// Provisional marks recent days still accepting measurements. Absence does
	// not certify that every older day has completed its final preparation.
	Provisional bool `json:"provisional,omitempty"`
}
