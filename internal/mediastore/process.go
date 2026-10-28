package mediastore

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
)

// ProcessJob asks the media worker to bring an item's outputs up to date:
// every producer recomputes its outputs' fingerprints and redoes the stale
// ones (and the pending ones), records them, and syncs public/. With Place,
// the worker first places the item's staged uploads (Manifests.Place).
type ProcessJob struct {
	Ref    contentref.ContentRef `json:"ref"`
	Commit string                `json:"commit,omitempty"` // admitted manifest operation completed by ingest
	Place  bool                  `json:"place,omitempty"`
	Preset string                `json:"preset,omitempty"` // only this preset; "" for all
	Force  bool                  `json:"force,omitempty"`  // redo current outputs too
	Editor bool                  `json:"editor,omitempty"` // also render missing editor views
	Class  media.VideoJobClass   `json:"class,omitempty"`
}

// ProcessQueue enqueues processing in the media worker (workqueue.Queue).
type ProcessQueue interface {
	Enqueue(ctx context.Context, job ProcessJob) error
}
