package signal

import (
	"fmt"
)

const (
	MaxSignalsPerBatch = 500
)

const (
	MaxExposuresPerBatch = 200
)

const (
	MaxShownPerExposure = 200
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
