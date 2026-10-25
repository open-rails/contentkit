package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// publicationOnlyFleet keeps entity synchronization stopped while allowing the
// real text publication worker to complete. Pending-scope tests add the entity
// queue explicitly only after proving that unprovisioned scopes grant nothing.
func publicationOnlyFleet(t *testing.T, app *application) *river.Client[pgx.Tx] {
	t.Helper()
	only := riverhelpers.NewContribution("critical-publication-only", func(_ context.Context, cfg *river.Config) error {
		cfg.Queues = map[string]river.QueueConfig{catalog.QueueFor(app.cfg.Scope.AppID.String()): {MaxWorkers: 1}}
		return nil
	}, nil, nil)
	fleet, err := riverhelpers.New(t.Context(), app.deps.Postgres, &river.Config{Schema: app.cfg.Database.Schema + "_river", Queues: map[string]river.QueueConfig{}}, app.engine.RiverJobs(), only)
	must(t, err)
	must(t, app.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
	must(t, fleet.Start(t.Context()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	return fleet
}
