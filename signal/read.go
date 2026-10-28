package signal

import (
	"time"
)

// DayActivity reports distinct people with measured attention, not page opens.
// Cohorts are application-defined; unmatched readers still count in Readers.
type DayActivity struct {
	Day      time.Time
	Readers  uint64
	Cohorts  map[string]uint64
	ActiveMS uint64
}
