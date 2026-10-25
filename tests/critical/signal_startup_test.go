package critical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	ck "github.com/open-rails/contentkit"
)

func TestPreparedSignalSchemaReadOnly(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	config := a.cfg.ClickHouse
	readOnly, err := clickhouse.Open(&clickhouse.Options{Addr: []string{config.Addr}, Auth: clickhouse.Auth{Database: config.Database, Username: config.Username, Password: config.Password}, Settings: clickhouse.Settings{"readonly": 1}, DialTimeout: time.Second})
	must(t, err)
	defer func() { must(t, readOnly.Close()) }()
	t.Run("CK-J01-S03_prepared_ClickHouse_startup_performs_no_DDL", func(t *testing.T) {
		err := readOnly.Exec(t.Context(), "CREATE TABLE must_not_exist (id UInt64) ENGINE=Memory")
		var denied *clickhouse.Exception
		if !errors.As(err, &denied) || denied.Code != 164 {
			t.Fatalf("ClickHouse did not enforce read-only mode: %v", err)
		}
		cfg, deps := a.cfg, a.deps
		cfg.Database.SkipMigrations = true
		deps.ClickHouse = readOnly
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		found := false
		for _, probe := range engine.Probes() {
			if probe.Name == "contentkit_clickhouse" {
				found = true
				must(t, probe.Check(t.Context()))
			}
		}
		if !found {
			t.Fatal("configured signal plane was silently disabled")
		}
		model, err := engine.Client().GetKinds(t.Context())
		must(t, err)
		if len(model.Kinds) != 2 {
			t.Fatal("read-only signal configuration lost the public model")
		}
		// The original ordinary connection stays borrowed and usable after the
		// read-only instance closes; the engine must not own either host handle.
		must(t, engine.Close(t.Context()))
		must(t, readOnly.Ping(t.Context()))
		must(t, a.deps.ClickHouse.Exec(t.Context(), "SELECT 1"))
	})
}
