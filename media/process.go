package media

// VideoJobClass selects the priority of a new video run. An empty class is
// an upload; backfills yield to new playable videos.
type VideoJobClass string

const (
	VideoBackfill VideoJobClass = "backfill"
)
