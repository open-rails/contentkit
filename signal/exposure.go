package signal

import (
	"context"
	"fmt"
	"strings"
)

// RecordExposures appends one row per result list and stage, batched into a
// single INSERT. Exposures of erased subjects are dropped. Empty input is a
// no-op.
func (st *Store) RecordExposures(ctx context.Context, tenant string, exposures []Exposure) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if len(exposures) == 0 {
		return nil
	}
	if len(exposures) > MaxExposuresPerBatch {
		return &LimitError{Field: "exposures per batch", Limit: MaxExposuresPerBatch, Got: len(exposures)}
	}
	subjects := make([]Subject, 0, len(exposures))
	for i := range exposures {
		if err := exposures[i].validate(); err != nil {
			return err
		}
		if exposures[i].Subject != (Subject{}) {
			subjects = append(subjects, exposures[i].Subject)
		}
	}
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil {
		return err
	}
	rows := make([]string, 0, len(exposures))
	args := make([]any, 0, len(exposures)*14)
	for i := range exposures {
		e := exposures[i]
		if _, erased := fenced[[2]string{e.Subject.Kind(), e.Subject.Key()}]; erased {
			continue
		}
		surface := strings.TrimSpace(e.Surface)
		if surface == "" {
			surface = SurfaceSearch
		}
		types := make([]string, len(e.Shown))
		ids := make([]string, len(e.Shown))
		positions := make([]uint32, len(e.Shown))
		for j, p := range e.Shown {
			types[j], ids[j], positions[j] = p.EntityType, p.EntityID, p.Position
		}
		kind, key := "", ""
		if e.Subject != (Subject{}) {
			kind, key = e.Subject.Kind(), e.Subject.Key()
		}
		rows = append(rows, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, tenant, e.RenderID, string(e.Stage), e.Revision, e.QueryID, surface, e.Ranker, e.Language,
			kind, key, types, ids, positions, e.OccurredAt.UTC())
	}
	if len(rows) == 0 {
		return nil
	}
	insert := fmt.Sprintf(`INSERT INTO %s.exposures
(tenant, render_id, stage, revision, query_id, surface, ranker, language, subject_kind, subject,
 entity_types, entity_ids, positions, occurred_at)
VALUES %s`, st.db, strings.Join(rows, ", "))
	if err := st.conn.Exec(ctx, insert, args...); err != nil {
		return fmt.Errorf("signal: insert exposures: %w", err)
	}
	return nil
}

// ForgetExposures removes all result-list exposures of one tenant and subject
// (host "clear my search history"). Not erasure: see EraseSubjects.
func (st *Store) ForgetExposures(ctx context.Context, tenant string, subject Subject) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	return st.mutate(ctx, "exposures", "tenant = ? AND subject_kind = ? AND subject = ?", tenant, subject.Kind(), subject.Key())
}
