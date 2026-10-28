package media

// VideoJobClass selects the priority of a new video run. An empty class is
// an upload; re-encodes and backfills yield to new playable videos.
type VideoJobClass string

const (
	VideoReencode VideoJobClass = "reencode"
)

const (
	VideoBackfill VideoJobClass = "backfill"
)
