package worker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// completionHook is part of the processing attempt. It records observable
// progress/completion and real metadata work before River completes the job.
// A failed transaction retries this same attempt; there is no delivery job.
type completionHook struct {
	river.HookDefaults
	manifests *media.Manifests
	publish   func(context.Context, pgx.Tx, contentref.ContentRef) error
}

func (h *completionHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) (result error) {
	workErr := err
	var snooze *river.JobSnoozeError
	checkpoint := job.Kind == (workqueue.VideoArgs{}).Kind() && errors.As(err, &snooze) && snooze.Duration == 0 && ctx.Err() == nil
	if err != nil && !checkpoint || job.Kind != (workqueue.IngestArgs{}).Kind() && job.Kind != (workqueue.ImageArgs{}).Kind() && !slices.Contains(workqueue.EncodeKinds, job.Kind) {
		return err
	}
	defer func() { result = media.SnoozeUnavailable(ctx, h.manifests.Store(), job, result) }()
	var args struct {
		Ref contentref.ContentRef `json:"ref"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return workErr
	}
	hooks := h.manifests.Registry().Config().Hooks
	err = media.CompleteProcessing(ctx, h.manifests, args.Ref, func(ctx context.Context, tx pgx.Tx, r media.Readiness) error {
		if err := h.publish(ctx, tx, args.Ref); err != nil {
			return err
		}
		// Existing custom embedded integrations keep their transaction contract.
		// Stock workers need no callbacks: facts/snapshots are version metadata work.
		if r.State == media.StateProcessing {
			if hooks.ItemProgressed != nil {
				k, err := h.manifests.Registry().Kind(args.Ref.ContentKind)
				if err != nil {
					return err
				}
				if k.Publish == nil {
					m, _, err := h.manifests.Get(ctx, args.Ref)
					if err != nil {
						return err
					}
					if !k.Playable(m) {
						return nil
					}
				}
				return hooks.ItemProgressed(ctx, tx, args.Ref)
			}
		} else if hooks.ItemReady != nil {
			return hooks.ItemReady(ctx, tx, args.Ref, r)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return workErr
}
