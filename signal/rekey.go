package signal

import (
	"context"
	"fmt"
)

// RekeyAnonymousSubject moves one quiesced anonymous subject's events and
// exposures to another, rebuilds its projections, then permanently erases
// the old key. The caller must stop writes to the old key and serialize this
// operation with erasure of both keys. It is safe to retry after interruption.
func (st *Store) RekeyAnonymousSubject(ctx context.Context, tenant string, old, next Subject) error {
	if tenant == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := old.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if old == next || old.AnonKey == "" || next.AnonKey == "" {
		return fmt.Errorf("signal: rekey requires distinct anonymous subjects")
	}

	fenced, err := st.fenced(ctx, tenant, []Subject{old, next})
	if err != nil {
		return err
	}
	if _, erased := fenced[[2]string{old.Kind(), old.Key()}]; erased {
		_, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{old})
		return err
	}
	if _, erased := fenced[[2]string{next.Kind(), next.Key()}]; erased {
		_, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{old})
		return err
	}

	copySignals := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id, revision, occurred_at,
 duration_s, progress, progress_max, value, score, completed, resume, payload, version, ingested_at)
SELECT tenant, content_kind, content_id, content_version_id, subject_kind, ?, signal_type, event_id, revision, occurred_at,
       duration_s, progress, progress_max, value, score, completed, resume, payload, version, now64(6)
FROM %s.signals
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND %s`, st.db, st.db, st.notErased(), st.subjectNotErased())
	if err := st.conn.Exec(ctx, copySignals, next.Key(), tenant, old.Kind(), old.Key(), tenant, subjectHashKey(next)); err != nil {
		return fmt.Errorf("signal: rekey events: %w", err)
	}

	copyExposures := fmt.Sprintf(`INSERT INTO %s.exposures
(tenant, render_id, stage, revision, query_id, surface, ranker, language, subject_kind, subject,
 content_kinds, content_ids, content_version_ids, positions, occurred_at, version, ingested_at)
SELECT tenant, render_id, stage, revision, query_id, surface, ranker, language, subject_kind, ?,
       content_kinds, content_ids, content_version_ids, positions, occurred_at, version, now64(6)
FROM %s.exposures
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND %s`, st.db, st.db, st.notErased(), st.subjectNotErased())
	if err := st.conn.Exec(ctx, copyExposures, next.Key(), tenant, old.Kind(), old.Key(), tenant, subjectHashKey(next)); err != nil {
		return fmt.Errorf("signal: rekey exposures: %w", err)
	}

	if err := st.reprojectSubject(ctx, tenant, next); err != nil {
		return err
	}
	if _, err := st.EraseSubjects(ctx, []string{tenant}, []Subject{old}); err != nil {
		return fmt.Errorf("signal: erase old subject after rekey: %w", err)
	}
	return nil
}

func (st *Store) reprojectSubject(ctx context.Context, tenant string, subject Subject) error {
	var after [3]string
	for {
		query := fmt.Sprintf(`SELECT DISTINCT content_kind, content_id, content_version_id
FROM %s.signals
WHERE tenant = ? AND subject_kind = ? AND subject = ?
  AND (content_kind, content_id, content_version_id) > (?, ?, ?)
ORDER BY content_kind, content_id, content_version_id LIMIT 256`, st.db)
		rows, err := st.conn.Query(ctx, query, tenant, subject.Kind(), subject.Key(), after[0], after[1], after[2])
		if err != nil {
			return fmt.Errorf("signal: list rekeyed content: %w", err)
		}
		keys := make([]ProjectionKey, 0, 256)
		for rows.Next() {
			var kind, id, version string
			if err := rows.Scan(&kind, &id, &version); err != nil {
				rows.Close()
				return fmt.Errorf("signal: scan rekeyed content: %w", err)
			}
			keys = append(keys, ProjectionKey{ContentKey: ContentKey{ContentKind: kind, ContentID: id, ContentVersionID: version}, Subject: subject})
			after = [3]string{kind, id, version}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("signal: list rekeyed content: %w", err)
		}
		if len(keys) == 0 {
			return nil
		}
		if err := st.project(ctx, tenant, keys); err != nil {
			return fmt.Errorf("signal: project rekeyed content: %w", err)
		}
	}
}
