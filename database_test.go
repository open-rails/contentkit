package contentkit

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationValidationErrorKeepsOutagesRetryable(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, &pgconn.PgError{Code: "57P01"}, &pgconn.PgError{Code: "53300"}, &pgconn.PgError{Code: "08006"}} {
		got := migrationValidationError(cause)
		if errors.Is(got, ErrMigrationsRequired) || !errors.Is(got, cause) {
			t.Fatalf("outage reclassified: %v", got)
		}
	}
	missing := &pgconn.PgError{Code: "42P01"}
	got := migrationValidationError(missing)
	if !errors.Is(got, ErrMigrationsRequired) || !errors.Is(got, missing) {
		t.Fatalf("missing schema not actionable: %v", got)
	}
}

type migrationProbe struct{ queries, execs []string }

func (p *migrationProbe) Exec(_ context.Context, q string, _ ...any) error {
	p.execs = append(p.execs, q)
	return nil
}
func (p *migrationProbe) Query(_ context.Context, q string, _ ...any) (driver.Rows, error) {
	p.queries = append(p.queries, q)
	return nil, errors.New("missing schema")
}
func TestSkippedSignalMigrationsNeverRunOnConnectOrRetry(t *testing.T) {
	e := &Engine{cfg: Config{Database: DatabaseConfig{SkipMigrations: true}, ClickHouse: ClickHouseConfig{Addr: "127.0.0.1:1", Database: "signals"}}}
	probe := &migrationProbe{}
	for range 2 {
		if _, err := e.connectSignals(t.Context(), probe); err == nil || !strings.Contains(err.Error(), "MigrateClickHouse") {
			t.Fatalf("missing schema guidance: %v", err)
		}
	}
	if len(probe.execs) != 0 || len(probe.queries) != 2 {
		t.Fatalf("connect/retry attempted migration: %+v", probe)
	}
}
