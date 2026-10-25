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
	queue := catalog.QueueFor(app.cfg.Scope.AppID.String())
	var dormant []string
	only := riverhelpers.NewContribution("critical-publication-only", func(_ context.Context, cfg *river.Config) error {
		for name := range cfg.Queues {
			if name != queue {
				dormant = append(dormant, name)
			}
		}
		return nil
	}, nil, nil)
	fleet, err := riverhelpers.New(t.Context(), app.deps.Postgres, &river.Config{Schema: app.cfg.Database.Schema + "_river", Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 1}}}, app.engine.RiverJobs(), only)
	must(t, err)
	// Contributions cannot overwrite another module's queue configuration.
	// Use River's public queue lifecycle before any consumer is started.
	for _, name := range dormant {
		must(t, fleet.Queues().Remove(t.Context(), name))
	}
	must(t, app.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
	must(t, fleet.Start(t.Context()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	return fleet
}
