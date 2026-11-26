// Package signal is the public surface of ContentKit's ClickHouse activity
// log: the connection it needs, its options and its errors. The log, its
// daily rollups and the job folding it into reading state are internal.
package signal

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Conn is the minimal ClickHouse surface the activity log needs;
// clickhouse-go's driver.Conn satisfies it. Implementations must forward the
// context unchanged so clickhouse-go receives query options, including
// settings and external tables.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// StoreOptions identifies the complete, single-shard replica set that pooled
// connections may use. Cluster runs DDL and erasure writes on every replica.
type StoreOptions struct{ Cluster string }
