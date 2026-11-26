package signalstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	signal "github.com/open-rails/contentkit/signal"
)

// MaxPreferenceBatch bounds one RecordPreferences insert.
const MaxPreferenceBatch = 5000

// RecordPreferences appends a batch of current-preference events as one
// plain multi-row INSERT … VALUES, which the server's async_insert can take.
// A backfill spans many months of occurred_at, so the batch may touch any
// number of partitions. The producer leaves out erased accounts and every
// read carries the erasure ledger; a subject the ledger covers once the rows
// have landed (an erasure racing the batch) has its signals deleted again,
// so the race leaves no residue. Re-sends converge on (event_id, revision).
func (st *Store) RecordPreferences(ctx context.Context, tenant string, events []signal.Signal) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if len(events) == 0 {
		return nil
	}
	if len(events) > MaxPreferenceBatch {
		return &signal.LimitError{Field: "preferences per batch", Limit: MaxPreferenceBatch, Got: len(events)}
	}
	const perRow = 12
	rows := make([]string, 0, len(events))
	args := make([]any, 0, len(events)*perRow)
	subjects := make([]signal.Subject, 0, len(events))
	for _, s := range events {
		if s.Measurement != nil || s.Type == signal.TypeEngagement {
			return fmt.Errorf("signal: measured engagement is not a preference event")
		}
		if err := validateSignal(s, tenant); err != nil {
			return err
		}
		payload, err := signalPayload(s)
		if err != nil {
			return err
		}
		rows = append(rows, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, tenant, s.ContentKind, s.ContentID, s.Version(), s.Subject.Kind(), s.Subject.Key(), s.Type,
			strings.TrimSpace(s.EventID), s.Revision, s.OccurredAt.UTC().Format(time.DateTime), s.Value, payload)
		subjects = append(subjects, s.Subject)
	}
	insert := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id, revision, occurred_at, value, payload)
VALUES %s`, st.db, strings.Join(rows, ", "))
	wide := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_partitions_per_insert_block": 0}))
	if err := st.conn.Exec(wide, insert, args...); err != nil {
		return fmt.Errorf("signal: insert preferences: %w", err)
	}
	return st.dropErased(ctx, tenant, subjects)
}

// dropErased deletes the signals of any given subject the erasure ledger
// covers. An erasure records its fence before deleting, so a write landing
// after that delete finds the fence here.
func (st *Store) dropErased(ctx context.Context, tenant string, subjects []signal.Subject) error {
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil || len(fenced) == 0 {
		return err
	}
	erased := make([]signal.Subject, 0, len(fenced))
	for k := range fenced {
		erased = append(erased, subjectFromKey(k[0], k[1]))
	}
	filter, args, err := subjectFilter(erased)
	if err != nil {
		return err
	}
	return st.mutate(ctx, "signals", "tenant = ? AND "+filter, append([]any{tenant}, args...)...)
}
