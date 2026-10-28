package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-rails/contentkit/internal/normalize"
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
		if utf8.RuneCountInString(normalize.Query(exposures[i].SearchQuery)) > 200 {
			return fmt.Errorf("exposure: search query exceeds 200 runes")
		}
		if err := validateExposure(exposures[i], tenant); err != nil {
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
	const perRow = 17
	rows := make([]string, 0, len(exposures))
	args := make([]any, 0, len(exposures)*perRow)
	sizes := make([]int, 0, len(exposures))
	for i := range exposures {
		e := exposures[i]
		if _, erased := fenced[[2]string{e.Subject.Kind(), e.Subject.Key()}]; erased {
			continue
		}
		surface := strings.TrimSpace(e.Surface)
		if surface == "" {
			surface = SurfaceSearch
		}
		kinds := make([]string, len(e.Shown))
		ids := make([]string, len(e.Shown))
		versions := make([]string, len(e.Shown))
		positions := make([]uint32, len(e.Shown))
		contexts := make([]string, len(e.Shown))
		for j, p := range e.Shown {
			kinds[j], ids[j], versions[j], positions[j] = p.ContentKind, p.ContentID, p.Version(), p.Position
			if p.Access != nil {
				encoded, err := json.Marshal(p.Access)
				if err != nil {
					return err
				}
				contexts[j] = string(encoded)
			}
		}
		kind, key := "", ""
		if e.Subject != (Subject{}) {
			kind, key = e.Subject.Kind(), e.Subject.Key()
		}
		// Keep the erasure predicate in the INSERT statement. The client-side
		// fenced lookup is only an optimization; an in-flight writer may resume
		// after EraseSubjects records its ledger row.
		rows = append(rows, "SELECT ? AS tenant, ? AS render_id, ? AS stage, ? AS revision, ? AS query_id, ? AS surface, ? AS ranker, ? AS language, ? AS subject_kind, ? AS subject, ? AS content_kinds, ? AS content_ids, ? AS content_version_ids, ? AS positions, ? AS occurred_at, ? AS access_contexts, ? AS search_query")
		args = append(args, tenant, e.RenderID, string(e.Stage), e.Revision, e.QueryID, surface, e.Ranker, e.Language,
			kind, key, kinds, ids, versions, positions, e.OccurredAt.UTC(), contexts, strings.ToLower(normalize.Query(e.SearchQuery)))
		sizes = append(sizes, inlinedSize(rows[len(rows)-1], args[len(args)-perRow:]))
	}
	for start, end := 0, 0; start < len(rows); start = end {
		end = insertEnd(sizes, start)
		insert := fmt.Sprintf(`INSERT INTO %s.exposures
(tenant, render_id, stage, revision, query_id, surface, ranker, language, subject_kind, subject,
 content_kinds, content_ids, content_version_ids, positions, occurred_at, access_contexts, search_query)
SELECT tenant, render_id, stage, revision, query_id, surface, ranker, language, subject_kind, subject,
       content_kinds, content_ids, content_version_ids, positions, occurred_at, access_contexts, search_query
FROM (%s) AS incoming
WHERE %s`, st.db, strings.Join(rows[start:end], " UNION ALL "), st.notErased())
		if err := st.conn.Exec(ctx, insert, args[start*perRow:end*perRow]...); err != nil {
			return fmt.Errorf("signal: insert exposures: %w", err)
		}
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

// ForgetExposuresBefore removes a subject's exposures through the second
// containing before. Exposure occurrence times are stored at second precision.
func (st *Store) ForgetExposuresBefore(ctx context.Context, tenant string, subject Subject, before time.Time) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	if before.IsZero() {
		return fmt.Errorf("signal: exposure clear time is required")
	}
	end := before.UTC().Truncate(time.Second).Add(time.Second)
	return st.mutate(ctx, "exposures", "tenant = ? AND subject_kind = ? AND subject = ? AND occurred_at < ?", tenant, subject.Kind(), subject.Key(), end)
}
