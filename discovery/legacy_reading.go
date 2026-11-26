package discovery

import (
	"time"
)

// ImportLegacyReading is an account's reading of a content item as a legacy
// source kept it, imported into its reading state. Removed deletes it.
type ImportLegacyReading struct {
	ContentID   string
	AccountID   string
	Removed     bool
	FirstReadAt *time.Time
	LastReadAt  time.Time
	// Progress is the page or second reached, of ProgressMax.
	Progress    uint32
	ProgressMax uint32
	Completed   bool
}
