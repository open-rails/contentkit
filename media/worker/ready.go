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
// through its media queue) after each place, image or video job that leaves
// the item settled, and its progress (Hooks.ItemProgressed) after each that
// published renditions and leaves it playable but still processing.
type readyHook struct {
	river.HookDefaults
	manifests *media.Manifests
	host      *media.HostQueue
}

func (h *readyHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) (result error) {
	workErr := err
	var snooze *river.JobSnoozeError
	if err != nil && !errors.As(err, &snooze) || job.Kind != (workqueue.PlaceArgs{}).Kind() && job.Kind != (workqueue.ImageArgs{}).Kind() && !slices.Contains(workqueue.EncodeKinds, job.Kind) {
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
	m, _, err := h.manifests.Get(ctx, args.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return workErr // deleted meanwhile
	} else if err != nil {
		return err
	}
	k, err := h.manifests.Registry().Kind(args.Ref.ContentKind)
	if err != nil {
		return err
	}
	// Video and audio jobs publish renditions, so only they can make
	// a processing item playable.
	if k.Readiness(m).State == media.StateProcessing && (!publishes(job.Kind) || !k.Playable(m)) {
		return workErr
	}
	if readyErr := h.host.Ready(ctx, args.Ref); readyErr != nil {
		return readyErr
	}
	return workErr
}

func publishes(kind string) bool {
	return kind == (workqueue.VideoArgs{}).Kind() || kind == (workqueue.AudioArgs{}).Kind()
}
