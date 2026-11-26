package mediastore

import (
	"context"
	"time"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
)

// ProcessJob asks the media worker to bring an item's outputs up to date:
// every producer recomputes its outputs' fingerprints and redoes the stale
// ones (and the pending ones), records them, and syncs public/. With Place,
// the worker first places the item's staged uploads (Manifests.Place).
type ProcessJob struct {
	Ref    contentref.ContentRef `json:"ref"`
	Place  bool                  `json:"place,omitempty"`
	Preset string                `json:"preset,omitempty"` // only this preset; "" for all
	Force  bool                  `json:"force,omitempty"`  // redo current outputs too
	Editor bool                  `json:"editor,omitempty"` // also render missing editor views
	Class  media.VideoJobClass   `json:"class,omitempty"`
	// Retry is set by the worker on an attempt after one that failed, timed
	// out or died: producers record each output on its own.
	Retry bool `json:"-"`
}

// ProcessQueue enqueues processing in the media worker (workqueue.Queue).
type ProcessQueue interface {
	Enqueue(ctx context.Context, job ProcessJob) error
}

// GCQueue schedules an item's garbage collection (Jobs.Collect) no later
// than at: an earlier schedule wins.
type GCQueue interface {
	ScheduleGC(ctx context.Context, ref contentref.ContentRef, at time.Time) error
}
