package signalstore

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	signal "github.com/open-rails/contentkit/signal"
)

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CreateDatabase creates the activity database (ON CLUSTER when cluster is
// set). Its tables come from migrations.ClickHouse; migratekit connects to
// the database, so it must exist first.
func CreateDatabase(ctx context.Context, conn signal.Exec, database, cluster string) error {
	if !identRe.MatchString(database) {
		return fmt.Errorf("signal: invalid database name %q", database)
	}
	onCluster := ""
	if cluster != "" {
		if !identRe.MatchString(cluster) {
			return fmt.Errorf("signal: invalid cluster name %q", cluster)
		}
		onCluster = " ON CLUSTER " + cluster
	}
	if err := conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+database+onCluster); err != nil {
		return fmt.Errorf("signal: create database: %w", err)
	}
	return nil
}

type tableSpec struct {
	engine, version, sortingKey, partitionKey string
	columns                                   [][2]string
}

const lc = "LowCardinality(String)"

// expectedSchema is what this version reads and writes: migrations/clickhouse.
var expectedSchema = map[string]tableSpec{
	"activity": {engine: "MergeTree", sortingKey: "tenant, subject, ts", partitionKey: "toDate(ts)",
		columns: [][2]string{{"tenant", lc}, {"subject", "UUID"}, {"ts", "DateTime64(3, 'UTC')"}, {"action", lc},
			{"content_kind", lc}, {"content_id", "UUID"}, {"domain", lc}, {"language", lc}, {"access", lc}, {"utc_offset", "Int16"},
			{"request_id", "UUID"}, {"surface", lc}, {"position", "UInt16"}, {"active_ms", "UInt32"}, {"furthest", "UInt32"},
			{"current", "UInt32"}, {"length", "UInt32"}, {"unit", lc}, {"value", "Int8"}, {"reason", lc}, {"query", "String"},
			{"results", "Array(UUID)"}, {"ingested_at", "DateTime64(3, 'UTC')"}}},
	"content_daily": {engine: "MergeTree", sortingKey: "tenant, domain, language, day, content_kind, content_id", partitionKey: "toYYYYMM(day)",
		columns: [][2]string{{"tenant", lc}, {"domain", lc}, {"language", lc}, {"day", "Date"}, {"content_kind", lc}, {"content_id", "UUID"},
			{"audience", "Bool"}, {"class", lc}, {"reaction", lc}, {"reason", lc}, {"people", "UInt32"}, {"generation", "DateTime64(3, 'UTC')"}}},
	"audience_daily": {engine: "MergeTree", sortingKey: "tenant, domain, language, day, access", partitionKey: "toYYYYMM(day)",
		columns: [][2]string{{"tenant", lc}, {"domain", lc}, {"language", lc}, {"day", "Date"}, {"access", lc}, {"people", "UInt32"},
			{"generation", "DateTime64(3, 'UTC')"}}},
	"erasures": {engine: "ReplacingMergeTree", version: "erased_at", sortingKey: "tenant, subject",
		columns: [][2]string{{"tenant", lc}, {"subject", "UUID"}, {"erased_at", "DateTime64(3, 'UTC')"}}},
}

// CheckSchema verifies, read-only, that the database has exactly the tables,
// columns, engines and keys this version uses; table existence alone is not
// compatibility. Needs SELECT on system.tables and system.columns.
func CheckSchema(ctx context.Context, conn signal.Conn, database string) error {
	if !identRe.MatchString(database) {
		return fmt.Errorf("signal: invalid database name %q", database)
	}
	type liveTable struct {
		engine, engineFull, sortingKey, partitionKey string
		columns                                      map[string]string
	}
	live := map[string]*liveTable{}
	rows, err := conn.Query(ctx, `SELECT name, engine, engine_full, sorting_key, partition_key FROM system.tables WHERE database = ?`, database)
	if err != nil {
		return fmt.Errorf("signal: read schema tables: %w", err)
	}
	for rows.Next() {
		var name string
		lt := &liveTable{columns: map[string]string{}}
		if err := rows.Scan(&name, &lt.engine, &lt.engineFull, &lt.sortingKey, &lt.partitionKey); err != nil {
			rows.Close()
			return fmt.Errorf("signal: scan schema table: %w", err)
		}
		live[name] = lt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("signal: read schema tables: %w", err)
	}
	rows, err = conn.Query(ctx, `SELECT table, name, type FROM system.columns WHERE database = ?`, database)
	if err != nil {
		return fmt.Errorf("signal: read schema columns: %w", err)
	}
	for rows.Next() {
		var table, name, typ string
		if err := rows.Scan(&table, &name, &typ); err != nil {
			rows.Close()
			return fmt.Errorf("signal: scan schema column: %w", err)
		}
		if lt, ok := live[table]; ok {
			lt.columns[name] = typ
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("signal: read schema columns: %w", err)
	}
	var problems []string
	names := make([]string, 0, len(expectedSchema))
	for name := range expectedSchema {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		want := expectedSchema[name]
		got, ok := live[name]
		if !ok {
			problems = append(problems, "missing table "+name)
			continue
		}
		if engine := strings.TrimPrefix(got.engine, "Replicated"); engine != want.engine {
			problems = append(problems, fmt.Sprintf("%s engine %s, want %s", name, got.engine, want.engine))
		}
		if want.version != "" {
			if v := engineVersionColumn(got.engineFull); v != want.version {
				problems = append(problems, fmt.Sprintf("%s version column %q, want %q", name, v, want.version))
			}
		}
		if got.sortingKey != want.sortingKey {
			problems = append(problems, fmt.Sprintf("%s ORDER BY (%s), want (%s)", name, got.sortingKey, want.sortingKey))
		}
		if got.partitionKey != want.partitionKey {
			problems = append(problems, fmt.Sprintf("%s PARTITION BY %q, want %q", name, got.partitionKey, want.partitionKey))
		}
		expected := map[string]bool{}
		for _, c := range want.columns {
			expected[c[0]] = true
			switch typ, ok := got.columns[c[0]]; {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s missing column %s", name, c[0]))
			case typ != c[1]:
				problems = append(problems, fmt.Sprintf("%s.%s type %s, want %s", name, c[0], typ, c[1]))
			}
		}
		var extra []string
		for col := range got.columns {
			if !expected[col] {
				extra = append(extra, col)
			}
		}
		slices.Sort(extra)
		for _, col := range extra {
			problems = append(problems, fmt.Sprintf("%s unexpected column %s", name, col))
		}
	}
	if len(problems) > 0 {
		return &signal.SchemaMismatchError{Database: database, Problems: problems}
	}
	return nil
}

// engineVersionColumn is a Replacing engine's last argument, its version
// column: recorded_at in ReplicatedReplacingMergeTree('/path', '{replica}', recorded_at).
func engineVersionColumn(engineFull string) string {
	open := strings.Index(engineFull, "(")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(engineFull); i++ {
		switch engineFull[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				args := strings.Split(engineFull[open+1:i], ",")
				last := strings.TrimSpace(args[len(args)-1])
				if strings.HasPrefix(last, "'") {
					return ""
				}
				return last
			}
		}
	}
	return ""
}
