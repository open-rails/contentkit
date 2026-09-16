package signal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/searchkit/internal/signaltest"
)

func TestIntegrationMigrationsMatchCheckSchema(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	conn := env.Fresh(t, testDB)
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatalf("fresh lineage must satisfy CheckSchema: %v", err)
	}
	// migratekit may re-run a partially applied migration: every statement must
	// be individually idempotent.
	env.Apply(t, conn)
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatalf("reapplied lineage: %v", err)
	}
}

func TestIntegrationCheckSchemaRuntimePrivileges(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	conn := env.Fresh(t, testDB)
	admin := env.Open(t, "")
	const user = "searchkit_signal_runtime_test"
	for _, stmt := range []string{
		"DROP USER IF EXISTS " + user,
		"CREATE USER " + user + " IDENTIFIED BY 'runtime'",
		"GRANT SELECT, INSERT ON " + testDB + ".* TO " + user,
		"GRANT SELECT ON system.tables TO " + user,
		"GRANT SELECT ON system.columns TO " + user,
	} {
		if err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP USER IF EXISTS "+user) })
	runtime := signaltest.Env{Addr: env.Addr, User: user, Password: "runtime"}.Open(t, testDB)
	if err := CheckSchema(ctx, runtime, testDB); err != nil {
		t.Fatalf("runtime credentials must validate the schema read-only: %v", err)
	}
	if err := runtime.Exec(ctx, "ALTER TABLE events ADD COLUMN IF NOT EXISTS probe UInt8"); err == nil {
		t.Fatal("runtime credentials unexpectedly hold DDL privileges")
	}
	_ = conn
}

func TestIntegrationCheckSchemaRefusesIncompatible(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		ddl  []string
		want []string
	}{
		"missing database": {want: []string{"missing table events", "missing table subject_daily"}},
		"changed column and extra column": {
			ddl: []string{
				"ALTER TABLE subject_state" + env.OnCluster() + " MODIFY COLUMN total_events UInt64",
				"ALTER TABLE exposures" + env.OnCluster() + " ADD COLUMN raw_query String",
			},
			want: []string{"subject_state.total_events type UInt64, want UInt32", "exposures unexpected column raw_query"},
		},
		"missing projection table": {
			ddl:  []string{"DROP TABLE subject_daily" + env.OnCluster() + " SYNC"},
			want: []string{"missing table subject_daily"},
		},
		"wrong version column": {
			ddl: []string{
				"DROP TABLE item_pairs" + env.OnCluster() + " SYNC",
				"CREATE TABLE item_pairs" + env.OnCluster() + " (tenant LowCardinality(String), entity_type_a LowCardinality(String), entity_id_a String, entity_type_b LowCardinality(String), entity_id_b String, strength Int64, refreshed_at DateTime('UTC')) ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{database}/{table}', '{replica}', strength) ORDER BY (tenant, entity_type_a, entity_id_a, entity_type_b, entity_id_b)",
			},
			want: []string{`item_pairs version column "strength", want "refreshed_at"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var conn Conn
			if name == "missing database" {
				admin := env.Open(t, "")
				env.Drop(t, admin, testDB)
				if err := CreateDatabase(ctx, admin, testDB, env.Cluster); err != nil {
					t.Fatal(err)
				}
				conn = admin
			} else {
				c := env.Fresh(t, testDB)
				for _, stmt := range tc.ddl {
					if err := c.Exec(ctx, stmt); err != nil {
						t.Fatalf("%s: %v", stmt, err)
					}
				}
				conn = c
			}
			err := CheckSchema(ctx, conn, testDB)
			var mismatch *SchemaMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("want SchemaMismatchError, got %v", err)
			}
			for _, want := range tc.want {
				found := false
				for _, p := range mismatch.Problems {
					found = found || strings.Contains(p, want)
				}
				if !found {
					t.Fatalf("missing problem %q in %v", want, mismatch.Problems)
				}
			}
		})
	}
}
