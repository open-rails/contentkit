package media

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
)

// ProcessJob asks the media worker to bring an item's outputs up to date:
// every producer recomputes its outputs' fingerprints and redoes the stale
// ones (and the pending ones), records them, and syncs public/.
type ProcessJob struct {
	Ref    contentref.ContentRef `json:"ref"`
	Preset string                `json:"preset,omitempty"` // only this preset; "" for all
	Force  bool                  `json:"force,omitempty"`  // redo current outputs too
	Editor bool                  `json:"editor,omitempty"` // also render missing editor views
	Class  VideoJobClass         `json:"class,omitempty"`
}

// VideoJobClass selects the priority of a new video run. An empty class is
// an upload; re-encodes and backfills yield to new playable videos.
type VideoJobClass string

const (
	VideoReencode VideoJobClass = "reencode"
	VideoBackfill VideoJobClass = "backfill"
)

// ProcessQueue enqueues processing in the media worker (workqueue.Queue).
type ProcessQueue interface {
	Enqueue(ctx context.Context, job ProcessJob) error
}
