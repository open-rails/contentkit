package content

import (
	"context"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

const (
	PreferenceAxisReaction = "reaction" // value -1 dislike / 0 neutral / 1 like
)

const (
	PreferenceAxisFavorite = "favorite" // value 1 favorited / 0 not
)

// DefaultPreferenceSyncOverlap is the default Options.PreferenceSyncOverlap.
const DefaultPreferenceSyncOverlap = 5 * time.Minute

// Preference is one exported row: an actor's current value on one axis of a
// canonical reference, with the revision and time of its last change.
type Preference struct {
	contentref.ContentRef
	ActorID   string
	Axis      string
	Value     int16
	Revision  int64
	UpdatedAt time.Time
}

// PreferenceSender durably writes one page of preferences; an error aborts the
// sync without advancing the watermark. Re-sends must be idempotent.
type PreferenceSender func(ctx context.Context, page []Preference) error

// PreferenceSyncReport reports one sync: the revision it scanned after and the
// rows it sent.
type PreferenceSyncReport struct {
	From int64
	Sent int
}
