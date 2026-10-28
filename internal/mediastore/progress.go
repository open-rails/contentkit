package mediastore

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
)

// ProgressSource reads encode progress (workqueue.NewProgressSource). The read
// API asks only when a visible video file is pending.
type ProgressSource interface {
	EncodeProgress(ctx context.Context, ref contentref.ContentRef) (media.EncodeStatus, error)
}
