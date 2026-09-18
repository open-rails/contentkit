// Package signaltest provisions disposable signal-plane ClickHouse databases
// for integration tests by applying the real migration lineage.
package signaltest

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/contentkit/migrations"
)

// Env configures the ClickHouse under test. Tests skip when Addr is empty.
// Replicated engines require Keeper; Cluster enables ON CLUSTER DDL.
type Env struct {
	Addr, User, Password, Cluster string
}

// FromEnv reads CONTENTKIT_TEST_CH_{ADDR,USER,PASSWORD,CLUSTER} or skips.
func FromEnv(t testing.TB) Env {
	t.Helper()
	e := Env{
		Addr:     os.Getenv("CONTENTKIT_TEST_CH_ADDR"),
		User:     os.Getenv("CONTENTKIT_TEST_CH_USER"),
		Password: os.Getenv("CONTENTKIT_TEST_CH_PASSWORD"),
		Cluster:  os.Getenv("CONTENTKIT_TEST_CH_CLUSTER"),
	}
	if e.Addr == "" {
		t.Skip("CONTENTKIT_TEST_CH_ADDR not set; skipping ClickHouse integration test")
	}
	if e.User == "" {
		e.User = "default"
	}
	return e
}

// Open connects with database as the default database ("" = none).
func (e Env) Open(t testing.TB, database string) driver.Conn {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{e.Addr},
		Auth: clickhouse.Auth{Database: database, Username: e.User, Password: e.Password},
	})
	if err != nil {
		t.Fatalf("clickhouse open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// OnCluster renders the ON CLUSTER clause ("" without a cluster).
func (e Env) OnCluster() string {
	if e.Cluster == "" {
		return ""
	}
	return " ON CLUSTER " + e.Cluster
}

// Drop removes the database and its Keeper replica metadata synchronously.
func (e Env) Drop(t testing.TB, conn driver.Conn, database string) {
	t.Helper()
	if err := conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+e.OnCluster()+" SYNC"); err != nil {
		t.Fatalf("drop %s: %v", database, err)
	}
}

// Migrations returns the lineage's statements in apply order, rendered the way
// migratekit chmigrate renders them ({{ON_CLUSTER}} expansion, one statement at
// a time).
func (e Env) Migrations(t testing.TB) [][]string {
	t.Helper()
	names, err := fs.Glob(migrations.SignalClickHouse, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	out := make([][]string, 0, len(names))
	for _, name := range names {
		b, err := fs.ReadFile(migrations.SignalClickHouse, name)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.ReplaceAll(string(b), "{{ON_CLUSTER}}", e.OnCluster())
		out = append(out, Split(content))
	}
	return out
}

// Apply runs every migration statement against conn (default database set).
func (e Env) Apply(t testing.TB, conn driver.Conn) {
	t.Helper()
	e.ApplyRange(t, conn, 0, len(e.Migrations(t)))
}

// ApplyRange runs migrations [from, to) (zero-based, lineage order).
func (e Env) ApplyRange(t testing.TB, conn driver.Conn, from, to int) {
	t.Helper()
	for _, stmts := range e.Migrations(t)[from:to] {
		for _, stmt := range stmts {
			if err := conn.Exec(context.Background(), stmt); err != nil {
				t.Fatalf("apply migration statement: %v\n%s", err, stmt)
			}
		}
	}
}

// Fresh drops and recreates database, applies all migrations and returns a
// connection whose default database is database.
func (e Env) Fresh(t testing.TB, database string) driver.Conn {
	t.Helper()
	conn := e.Empty(t, database)
	e.Apply(t, conn)
	return conn
}

// Empty drops and recreates database without applying migrations.
func (e Env) Empty(t testing.TB, database string) driver.Conn {
	t.Helper()
	admin := e.Open(t, "")
	e.Drop(t, admin, database)
	if err := admin.Exec(context.Background(), "CREATE DATABASE "+database+e.OnCluster()); err != nil {
		t.Fatalf("create %s: %v", database, err)
	}
	return e.Open(t, database)
}

// Split separates statements terminated by ';' at end of line, dropping
// comment-only lines. Migration SQL keeps semicolons out of literals.
func Split(content string) []string {
	var (
		out []string
		cur strings.Builder
	)
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSuffix(strings.TrimSpace(cur.String()), ";")
			out = append(out, stmt)
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}
