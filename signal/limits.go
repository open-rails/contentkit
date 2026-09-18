package signal

import "fmt"

// Ingestion bounds. Oversize input is rejected, never truncated, so hosts see
// it as backpressure and can count it instead of silently losing detail.
const (
	MaxSignalsPerBatch   = 500
	MaxExposuresPerBatch = 200
	MaxShownPerExposure  = 200
	// MaxIdentifierBytes bounds content kinds/ids/versions, subject keys,
	// signal types, event ids, query ids, surfaces and languages.
	MaxIdentifierBytes = 256
	MaxResumeBytes     = 256
	MaxQueryBytes      = 256
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

func checkLen(field, value string, limit int) error {
	if len(value) > limit {
		return &LimitError{Field: field, Limit: limit, Got: len(value)}
	}
	return nil
}

func checkIdentifiers(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := checkLen(pairs[i], pairs[i+1], MaxIdentifierBytes); err != nil {
			return err
		}
	}
	return nil
}

// MaxErasureSubjects bounds one EraseSubjects call.
const MaxErasureSubjects = 100
