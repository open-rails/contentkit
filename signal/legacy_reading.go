package signal

import (
	"time"
)

// LegacyReading restores an existing bookmark/progress fact. It carries no
// attendance, coverage or quality evidence and never contributes to readership,
// completion statistics, popularity or recommendation seeds.
type LegacyReading struct {
	ContentRef
	Subject     Subject
	FirstReadAt time.Time
	LastReadAt  time.Time
	Progress    uint32
	ProgressMax uint32
	Completed   bool
	Resume      string
	Revision    uint64
}
