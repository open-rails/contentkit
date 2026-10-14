package media

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
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

// ProcessCanceler is a ProcessQueue that can cancel an item's queued and
// running jobs (workqueue.Queue): a commit removing an upload still being
// processed cancels them, then enqueues the item's remaining work.
type ProcessCanceler interface {
	Cancel(ctx context.Context, ref contentref.ContentRef) (int, error)
}

// FrameGrabber grabs a JPEG still at t seconds (width 0 keeps the frame's)
// from a video upload, for the frame picker (Uploads.Frame); media/video's
// Frames implements it.
type FrameGrabber interface {
	Frame(ctx context.Context, item Item, video File, t float64, width int) ([]byte, error)
}
