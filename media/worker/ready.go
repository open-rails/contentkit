package worker

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// readyHook calls Hooks.ItemReady after each image or video job that leaves
// its item settled (media.Root.Readiness not processing).
type readyHook struct {
	river.HookDefaults
	pool      *pgxpool.Pool
	manifests *media.Manifests
	ready     func(context.Context, pgx.Tx, contentref.ContentRef, media.Readiness) error
}

func (h *readyHook) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) error {
	if err != nil || job.Kind != (workqueue.ImageArgs{}).Kind() && job.Kind != (workqueue.VideoArgs{}).Kind() {
		return err
	}
	var args struct {
		Ref contentref.ContentRef `json:"ref"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return nil // the job itself decoded them
	}
	return h.settle(ctx, args.Ref.Content())
}

// settle reports ref to the hook when its processing has settled.
func (h *readyHook) settle(ctx context.Context, ref contentref.ContentRef) error {
	r, err := h.manifests.Readiness(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil // deleted meanwhile
	} else if err != nil || r.State == media.StateProcessing {
		return err
	}
	return pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error { return h.ready(ctx, tx, ref, r) })
}
