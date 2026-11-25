package signal

import (
	"fmt"
)

// A batch at these limits, every item carrying an activity receipt, fits
// the 1 MiB request body.
const (
	MaxSignalsPerBatch = 500
)

const (
	MaxExposuresPerBatch = 8
)

const (
	// MaxShownPerExposure is a full list page.
	MaxShownPerExposure = 100
)

const (
	// MaxIdentifierBytes bounds content kinds/ids/versions, subject keys,
	// signal types, event ids, query ids, surfaces and languages.
	MaxIdentifierBytes = 256
)

const (
	MaxResumeBytes = 256
)

const (
	MaxQueryBytes = 256
)

const (
	// MaxPayloadBytes bounds the JSON-encoded payload.
	MaxPayloadBytes = 2048
)

// LimitError reports input that exceeds an ingestion bound.
type LimitError struct {
	Field string
	Limit int
	Got   int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("signal: %s is %d, limit %d", e.Field, e.Got, e.Limit)
}

// MaxErasureSubjects bounds one EraseSubjects call.
const MaxErasureSubjects = 100
