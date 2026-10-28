package discovery

import (
	"time"

	"github.com/open-rails/contentkit/signal"
)

// ImportSignal is a recorded event as the bulk import replays it: its
// subject given, scored as RecordSignals scores a client's.
type ImportSignal struct {
	ContentID  string
	VersionID  string
	Subject    signal.Subject
	Type       string
	EventID    string
	Revision   uint64
	OccurredAt time.Time
}
