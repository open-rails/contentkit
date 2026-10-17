package catalog

import (
	"context"
	"fmt"
)

// RetireOpenCounters removes the unused imported view-count columns. Call only
// after every app/importer sharing this catalog has upgraded; never on startup.
// Source images, content and reading resume/progress are unaffected.
func (s *Store) RetireOpenCounters(ctx context.Context, legacyWritersStopped bool) error {
	if !legacyWritersStopped {
		return fmt.Errorf("catalog: stop legacy writers before retiring imported counters")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, table := range []string{s.t.items, s.t.facets} {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+table+" DROP COLUMN IF EXISTS view_baseline"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
