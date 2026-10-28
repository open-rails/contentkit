package signal

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Conn is the minimal ClickHouse surface the Store needs; clickhouse-go's
// driver.Conn satisfies it. Implementations must forward the context unchanged
// so clickhouse-go receives query options, including external tables.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// StoreOptions identifies the complete, single-shard replica set that pooled
// connections may use. Cluster is needed for account anonymization when the
// signal tables have more than one replica.
type StoreOptions struct{ Cluster string }

// ProjectionKey identifies one subject × content reference projection.
type ProjectionKey struct {
	ContentKey
	Subject Subject
}

// RepairOptions bounds one RepairProjections call.
type RepairOptions struct {
	// Window limits candidate keys to those with an event on these days
	// (zero = all time).
	Window Window
	// IngestedSince limits candidates to keys with a row ingested at or after
	// it: the crash-repair scan. Zero = no bound.
	IngestedSince time.Time
	// After resumes strictly after this key in key order (nil = start).
	After *ProjectionKey
	// Limit caps candidate keys examined per call (default 1000).
	Limit int
	// Rebuild reprojects every candidate instead of only stale ones: after a
	// projection semantics change, a restore or a legacy import.
	Rebuild bool
}

// RepairResult reports one bounded repair step.
type RepairResult struct {
	Examined int
	Repaired int
	// Next is the cursor for the following call; nil when the range is done.
	Next *ProjectionKey
}

// ForgottenContent names purged content: a whole work (VersionID empty) or
// one version of it.
type ForgottenContent struct {
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}
