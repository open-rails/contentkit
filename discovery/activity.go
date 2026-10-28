package discovery

import (
	"time"
)

// ActivitySigner signs domain-separated activity receipts. The media reader
// implements it with its persistent delivery key; non-media hosts may supply
// their own signer. No signing key is generated implicitly.
type ActivitySigner interface {
	SignTicket(scope string, expires time.Time) string
	VerifyTicket(ticket, scope string) error
}

// ActivityTarget is the content a client is about to display or consume.
type ActivityTarget struct {
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

type ActivityContextsInput struct {
	Session  string           `json:"session,omitempty"`
	Language string           `json:"language,omitempty"`
	Targets  []ActivityTarget `json:"targets"`
}

// ActivityContext is an opaque, subject-bound snapshot. OccurredAt is the
// server's observation time and stays fixed across every view checkpoint.
type ActivityContext struct {
	ActivityTarget
	Token      string    `json:"token"`
	OccurredAt time.Time `json:"occurred_at"`
}

type ActivityContexts struct {
	Enabled  bool              `json:"enabled"`
	Contexts []ActivityContext `json:"contexts"`
}
