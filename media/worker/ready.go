package worker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// readyHook asks the host to report an item's readiness (Hooks.ItemReady,
// through its media queue) after each ingest, image or video job that leaves
// the item settled, and its progress (Hooks.ItemProgressed) after each that
// changes reader files or leaves a video playable while still processing.
type readyHook struct {
	river.HookDefaults
	manifests *media.Manifests
	host      *media.HostQueue
}

func (h *readyHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) (result error) {
	workErr := err
	var snooze *river.JobSnoozeError
	checkpoint := job.Kind == (workqueue.VideoArgs{}).Kind() && errors.As(err, &snooze) && snooze.Duration == 0 && ctx.Err() == nil
	if err != nil && !checkpoint || job.Kind != (workqueue.IngestArgs{}).Kind() && job.Kind != (workqueue.ImageArgs{}).Kind() && !slices.Contains(workqueue.EncodeKinds, job.Kind) {
		return err
	}
	// Output can become visible before this final manifest read. An outage
	// here must not spend a retry after the worker already handled its work.
	defer func() { result = media.SnoozeUnavailable(ctx, h.manifests.Store(), job, result) }()
	var args struct {
		Ref contentref.ContentRef `json:"ref"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return workErr // the job itself decoded them
	}
	m, etag, err := h.manifests.Get(ctx, args.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return workErr // deleted meanwhile
	} else if err != nil {
		return err
	}
	k, err := h.manifests.Registry().Kind(args.Ref.ContentKind)
	if err != nil {
		return err
	}
	// Reader snapshots can become ready before unrelated media finishes.
	// Player-only items still report partial progress only once playable.
	if k.Readiness(m).State == media.StateProcessing && k.Publish == nil && (!publishes(job.Kind) || !k.Playable(m)) {
		return workErr
	}
	// A video job yields after each bounded range. Only a new manifest
	// publication needs a host relay; remember it only after enqueue succeeds.
	var delivered struct {
		ETag string `json:"contentkit_ready_etag"`
	}
	video := job.Kind == (workqueue.VideoArgs{}).Kind()
	if video {
		if err := json.Unmarshal(job.Metadata, &delivered); err != nil {
			return err
		}
		if delivered.ETag == etag {
			return workErr
		}
	}
	if readyErr := h.host.Ready(ctx, args.Ref); readyErr != nil {
		return readyErr
	}
	if video {
		if err := river.MetadataSet(ctx, "contentkit_ready_etag", etag); err != nil {
			return err
		}
	}
	return workErr
}

func publishes(kind string) bool {
	return kind == (workqueue.VideoArgs{}).Kind()
}
