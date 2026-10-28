package discovery

import (
	"time"

	"github.com/open-rails/contentkit/signal"
)

type ImportLegacyReading struct {
	ContentID   string
	VersionID   string
	Subject     signal.Subject
	FirstReadAt *time.Time
	LastReadAt  time.Time
	Progress    uint32
	ProgressMax uint32
	Completed   bool
	Resume      string
	Revision    uint64
}
