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
// through its media queue) after each image or video job that leaves the
// item settled.
type readyHook struct {
	river.HookDefaults
	manifests *media.Manifests
	host      *media.HostQueue
}

func (h *readyHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) error {
	if err != nil || job.Kind != (workqueue.ImageArgs{}).Kind() && !slices.Contains(workqueue.EncodeKinds, job.Kind) {
		return err
	}
	var args struct {
		Ref contentref.ContentRef `json:"ref"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return nil // the job itself decoded them
	}
	m, _, err := h.manifests.Get(ctx, args.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil // deleted meanwhile
	} else if err != nil {
		return err
	}
	k, err := h.manifests.Registry().Kind(args.Ref.ContentKind)
	if err != nil || k.Readiness(m).State == media.StateProcessing {
		return err
	}
	return h.host.Ready(ctx, args.Ref)
}
