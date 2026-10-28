package signal

import (
	"context"
	"fmt"
	"strings"
)

// Exec is the minimal ClickHouse execution surface CreateDatabase needs;
// clickhouse-go's driver.Conn satisfies it.
type Exec interface {
	Exec(ctx context.Context, query string, args ...any) error
}

// SchemaMismatchError lists every difference between the live signal database
// and the schema this library version requires.
type SchemaMismatchError struct {
	Database string
	Problems []string
}

func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("signal: database %q is incompatible with this contentkit version (apply migrations.ClickHouse): %s",
		e.Database, strings.Join(e.Problems, "; "))
}
