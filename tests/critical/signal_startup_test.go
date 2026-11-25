package critical_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"
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

// The fixture's ClickHouse also serves its secure native port, with a
// certificate signed by the CA in CONTENTKIT_CRITICAL_CH_CA_FILE.
func TestClickHouseOverTLS(t *testing.T) {
	addr, caFile := os.Getenv("CONTENTKIT_CRITICAL_CH_TLS_ADDR"), os.Getenv("CONTENTKIT_CRITICAL_CH_CA_FILE")
	user, password := os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")
	if addr == "" || caFile == "" || user == "" || password == "" {
		t.Fatal("owned TLS ClickHouse required for the TLS signal plane")
	}
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J01-S14_ClickHouse_migrations_and_signals_run_over_TLS", func(t *testing.T) {
		cfg := a.cfg
		cfg.ClickHouse = ck.ClickHouseConfig{Addr: addr, Username: user, Password: password, Database: w.schema + "_tls_signals",
			Cluster: os.Getenv("CONTENTKIT_CRITICAL_CH_CLUSTER"), TLS: true, CAFile: caFile}
		pem, err := os.ReadFile(caFile)
		must(t, err)
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatalf("%s holds no PEM certificate", caFile)
		}
		admin, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, TLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			Auth: clickhouse.Auth{Username: user, Password: password}, DialTimeout: 5 * time.Second})
		must(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			must(t, admin.Exec(ctx, "DROP DATABASE IF EXISTS "+cfg.ClickHouse.Database+" SYNC"))
			must(t, admin.Close())
		})

		// The system roots do not hold the fixture's CA: nothing is created.
		untrusted := cfg
		untrusted.ClickHouse.CAFile = ""
		if err := ck.MigrateClickHouse(t.Context(), w.pool, untrusted); err == nil || !strings.Contains(err.Error(), "x509") {
			t.Fatalf("an unverified server was migrated: %v", err)
		}
		var exists uint8
		must(t, admin.QueryRow(t.Context(), "EXISTS DATABASE "+cfg.ClickHouse.Database).Scan(&exists))
		if exists != 0 {
			t.Fatal("an unverified dial created the signal database")
		}

		must(t, ck.MigrateClickHouse(t.Context(), w.pool, cfg))
		// Embedded migrations replay over TLS; the engine's own connection is TLS too.
		engine, err := ck.New(t.Context(), cfg, a.deps)
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
			t.Fatal("the TLS signal plane was silently disabled")
		}
		model, err := engine.Client().GetKinds(t.Context())
		must(t, err)
		if !model.Signals {
			t.Fatal("the content model reports signals off over TLS")
		}
	})
}

// A host supervisor polls contentkit_clickhouse every few seconds; the probe
// reports the plane's state and sends ClickHouse no query of its own.
func TestSignalProbeSendsNoQuery(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	config := a.cfg.ClickHouse
	client := "ck-probe-" + w.schema
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{config.Addr}, Auth: clickhouse.Auth{Database: config.Database, Username: config.Username, Password: config.Password},
		ClientInfo: clickhouse.ClientInfo{Products: []struct{ Name, Version string }{{Name: client, Version: "1"}}}, DialTimeout: time.Second})
	must(t, err)
	defer func() { must(t, conn.Close()) }()
	t.Run("CK-J01-S16_the_ClickHouse_probe_sends_no_query", func(t *testing.T) {
		cfg, deps := a.cfg, a.deps
		cfg.Database.SkipMigrations = true
		deps.ClickHouse = conn
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		var check func(context.Context) error
		for _, probe := range engine.Probes() {
			if probe.Name == "contentkit_clickhouse" {
				check = probe.Check
			}
		}
		if check == nil {
			t.Fatal("configured signal plane was silently disabled")
		}
		for range 20 {
			must(t, check(t.Context()))
		}
		must(t, conn.Exec(t.Context(), "SYSTEM FLUSH LOGS"))
		var probes uint64
		must(t, conn.QueryRow(t.Context(), `SELECT count() FROM system.query_log WHERE event_date >= yesterday()
			AND type = 'QueryFinish' AND position(client_name, ?) > 0 AND match(query, '^\\s*SELECT\\s+1\\s*$')`, client).Scan(&probes))
		if probes != 0 {
			t.Fatalf("20 probe checks sent ClickHouse %d queries", probes)
		}
	})
}
