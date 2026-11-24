package signalstore

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// A read serving a request is bounded so a burst cannot hold the pool: one
// arriving without a deadline gets readTimeout, which the driver also sends
// as max_execution_time, and every read gets a memory ceiling. A caller's
// own deadline, such as a job's, stands.
const (
	readTimeout = 10 * time.Second
	readMemory  = 1 << 30
)

func readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, readTimeout)
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_memory_usage": readMemory})), cancel
}
