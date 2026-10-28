package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/jobhook"

	ck "github.com/open-rails/contentkit"
	"github.com/riverqueue/river"
)

func TestSignalRecoverySchedulesPreferences(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	// Only this owned database is made temporarily unready. The real schema
	// probe, background reconnect and catch-up path all execute unchanged.
	db := a.cfg.ClickHouse.Database
	must(t, a.deps.ClickHouse.Exec(t.Context(), "RENAME TABLE "+db+".engagement_daily TO "+db+".ck257_held_daily"))
	restored := false
	restore := func() {
		if restored {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		must(t, a.deps.ClickHouse.Exec(ctx, "RENAME TABLE "+db+".ck257_held_daily TO "+db+".engagement_daily"))
		restored = true
	}
	t.Cleanup(restore)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	var check func(context.Context) error
	for _, probe := range a.engine.Probes() {
		if probe.Name == "contentkit_clickhouse" {
			check = probe.Check
		}
	}
	if check == nil || check(t.Context()) == nil {
		t.Fatal("missing real summary table did not make signal plane unready")
	}
	_, err = jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}})
	must(t, err)
	// The fleet deliberately never starts: a preference job after restoration
	// can only be the engine's reconnect callback, not a periodic first tick.
	restore()
	waitForWithin(t, 30*time.Second, func() bool {
		if check(t.Context()) != nil {
			return false
		}
		var preferences int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job WHERE kind='contentkit_preference_sync'`).Scan(&preferences))
		return preferences > 0
	})
}
