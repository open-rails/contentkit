package signal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Exec is the minimal ClickHouse execution surface CreateDatabase needs;
// clickhouse-go's driver.Conn satisfies it.
type Exec interface {
	Exec(ctx context.Context, query string, args ...any) error
}

// CreateDatabase creates the dedicated signal database (ON CLUSTER when cluster
// is set). The schema itself is owned by the versioned migrations in
// migrations.SignalClickHouse, applied with migratekit chmigrate; migratekit
// connects to the database, so it must exist first.
func CreateDatabase(ctx context.Context, conn Exec, database, cluster string) error {
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

type columnSpec struct{ name, typ string }

type tableSpec struct {
	engine       string // without the Replicated prefix
	version      string // ReplacingMergeTree version column
	sortingKey   string
	partitionKey string
	columns      []columnSpec
}

// expectedSchema is the schema this library version reads and writes. It must
// match the latest migration in migrations/clickhouse/signal.
var expectedSchema = map[string]tableSpec{
	"events": {
		engine: "ReplacingMergeTree", version: "version",
		sortingKey:   "tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id",
		partitionKey: "toYYYYMM(occurred_at)",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"entity_type", "LowCardinality(String)"}, {"entity_id", "String"},
			{"subject_kind", "LowCardinality(String)"}, {"subject", "String"}, {"signal_type", "LowCardinality(String)"},
			{"event_id", "String"}, {"revision", "UInt64"}, {"occurred_at", "DateTime('UTC')"}, {"duration_s", "UInt32"},
			{"progress", "UInt32"}, {"progress_max", "UInt32"}, {"value", "Float64"}, {"score", "Int16"},
			{"completed", "Bool"}, {"resume", "String"}, {"payload", "String"}, {"version", "UInt128"},
			{"ingested_at", "DateTime64(6, 'UTC')"},
		},
	},
	"subject_state": {
		engine: "ReplacingMergeTree", version: "version",
		sortingKey: "tenant, subject_kind, subject, entity_type, entity_id",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"subject_kind", "LowCardinality(String)"}, {"subject", "String"},
			{"entity_type", "LowCardinality(String)"}, {"entity_id", "String"}, {"first_seen_at", "DateTime('UTC')"},
			{"last_signal_at", "DateTime('UTC')"}, {"total_events", "UInt32"}, {"views", "UInt32"},
			{"completions", "UInt32"}, {"active_s", "UInt64"}, {"max_progress", "UInt32"}, {"progress_max", "UInt32"},
			{"completed", "Bool"}, {"resume", "String"}, {"last_score", "Int16"}, {"net_value", "Float64"},
			{"feedback", "UInt32"}, {"version", "DateTime64(6, 'UTC')"},
		},
	},
	"subject_daily": {
		engine: "ReplacingMergeTree", version: "version",
		sortingKey:   "tenant, entity_type, entity_id, subject_kind, subject, day",
		partitionKey: "toYYYYMM(day)",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"entity_type", "LowCardinality(String)"}, {"entity_id", "String"},
			{"subject_kind", "LowCardinality(String)"}, {"subject", "String"}, {"day", "Date"}, {"events", "UInt32"},
			{"views", "UInt32"}, {"completions", "UInt32"}, {"active_s", "UInt64"}, {"score_sum", "Int64"},
			{"value_sum", "Float64"}, {"type_counts", "Map(LowCardinality(String), UInt32)"},
			{"version", "DateTime64(6, 'UTC')"},
		},
	},
	"erasures": {
		engine: "ReplacingMergeTree", version: "erased_at",
		sortingKey: "tenant, subject_hash",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"subject_hash", "FixedString(16)"}, {"erased_at", "DateTime64(6, 'UTC')"},
		},
	},
	"item_pairs": {
		engine: "ReplacingMergeTree", version: "refreshed_at",
		sortingKey: "tenant, entity_type_a, entity_id_a, entity_type_b, entity_id_b",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"entity_type_a", "LowCardinality(String)"}, {"entity_id_a", "String"},
			{"entity_type_b", "LowCardinality(String)"}, {"entity_id_b", "String"}, {"strength", "Int64"},
			{"refreshed_at", "DateTime('UTC')"},
		},
	},
	"exposures": {
		engine: "ReplacingMergeTree", version: "version",
		sortingKey:   "tenant, render_id, stage",
		partitionKey: "toYYYYMM(occurred_at)",
		columns: []columnSpec{
			{"tenant", "LowCardinality(String)"}, {"render_id", "String"}, {"stage", "LowCardinality(String)"},
			{"revision", "UInt64"}, {"query_id", "String"}, {"surface", "LowCardinality(String)"},
			{"ranker", "LowCardinality(String)"}, {"language", "LowCardinality(String)"},
			{"subject_kind", "LowCardinality(String)"}, {"subject", "String"},
			{"entity_types", "Array(LowCardinality(String))"}, {"entity_ids", "Array(String)"},
			{"positions", "Array(UInt32)"}, {"occurred_at", "DateTime('UTC')"}, {"version", "UInt128"},
			{"ingested_at", "DateTime64(6, 'UTC')"},
		},
	},
}

// SchemaMismatchError lists every difference between the live signal database
// and the schema this library version requires.
type SchemaMismatchError struct {
	Database string
	Problems []string
}

func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("signal: database %q is incompatible with this searchkit version (apply migrations.SignalClickHouse): %s",
		e.Database, strings.Join(e.Problems, "; "))
}

// CheckSchema verifies, read-only, that the signal database has exactly the
// tables, columns, engines and keys this library version uses. Hosts call it at
// startup with runtime credentials and must not serve analytics on error: table
// existence alone is not compatibility. Requires SELECT on system.tables and
// system.columns for the database.
func CheckSchema(ctx context.Context, conn Conn, database string) error {
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("signal: read schema tables: %w", err)
	}
	rows.Close()

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
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("signal: read schema columns: %w", err)
	}
	rows.Close()

	var problems []string
	for _, name := range sortedKeys(expectedSchema) {
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
		if want.engine == "MaterializedView" {
			continue
		}
		expected := map[string]string{}
		for _, c := range want.columns {
			expected[c.name] = c.typ
			switch typ, ok := got.columns[c.name]; {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s missing column %s", name, c.name))
			case typ != c.typ:
				problems = append(problems, fmt.Sprintf("%s.%s type %s, want %s", name, c.name, typ, c.typ))
			}
		}
		for _, col := range sortedKeys(got.columns) {
			if _, ok := expected[col]; !ok {
				problems = append(problems, fmt.Sprintf("%s unexpected column %s", name, col))
			}
		}
	}
	if len(problems) > 0 {
		return &SchemaMismatchError{Database: database, Problems: problems}
	}
	return nil
}

// engineVersionColumn returns the last engine argument of a Replacing engine
// (its version column), e.g. "recorded_at" from
// ReplicatedReplacingMergeTree('/path', '{replica}', recorded_at) ORDER BY ...
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
