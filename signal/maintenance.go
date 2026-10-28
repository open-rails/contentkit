package signal

import (
	"time"
)

// InventoryRow sizes one content kind × signal type of a tenant's canonical
// events: the evidence for deciding which signals to keep collecting.
type InventoryRow struct {
	ContentKind string
	SignalType  string
	Events      uint64 // canonical events
	RawRows     uint64 // stored rows incl. unmerged superseded revisions and retries
	Subjects    uint64
	// ContentItems counts distinct content ids (works and their versions
	// count once per id).
	ContentItems uint64
	FirstAt      time.Time
	LastAt       time.Time
}
