package versions

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// ProcessingPublisher inserts real version metadata work directly from a
// processor's completion transaction, including stock external workers. It
// preserves a follow-up when an older snapshot job is already running.
func ProcessingPublisher(pool *pgxpool.Pool, schema string, registry *media.Registry) (func(context.Context, pgx.Tx, contentref.ContentRef) error, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
		k, err := registry.Kind(ref.ContentKind)
		if err != nil {
			return err
		}
		if !k.Versions || ref.Version() == "" {
			return nil
		}
		if _, err := registry.Item(ref); err != nil {
			return err
		}
		return media.InsertOnce(ctx, func(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
			return client.InsertTx(ctx, tx, args, opts)
		}, publishArgs{Content: ref.ContentID, Version: ref.Version()}, river.InsertOpts{Queue: Queue})
	}, nil
}
