package signal

import (
	"context"
	"fmt"
	"strings"
	"time"
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

// AttributionOptions selects an evaluation export.
type AttributionOptions struct {
	// Window bounds exposure and click days (zero = all time).
	Window Window
	// Stage is the exposure stage clicks are attributed against (required).
	Stage ExposureStage
	// Surface restricts renders to one surface (empty = all).
	Surface string
	// After resumes strictly after this render id (cursor).
	After string
	// Limit caps renders per page (default 500).
	Limit int
}

// AttributedClick is a canonical click carrying a render id.
type AttributedClick struct {
	EntityRef
	Position   uint32 // position claimed by the click
	OccurredAt time.Time
	EventID    string
	// Exposed reports whether the clicked entity is in the render's list at the
	// requested stage. False means "not exposed at this stage", never negative.
	Exposed bool
}

// AttributedRender is one render at one stage with its clicks joined.
type AttributedRender struct {
	RenderID   string
	QueryID    string
	Surface    string
	Ranker     string
	Language   string
	Subject    Subject
	OccurredAt time.Time
	Shown      []Placement
	Clicks     []AttributedClick
}

// AttributionPage is one page of the evaluation export.
type AttributionPage struct {
	Renders []AttributedRender
	// Unattributed are clicks in the window whose render has no exposure at
	// the requested stage (prefetched, cancelled, hidden or never recorded):
	// they cannot label anything.
	Unattributed []AttributedClick
	// Next is the cursor for the following page; empty when done.
	Next string
}

// Attribution joins canonical clicks to canonical exposures at one stage, so
// evaluation sees what was actually exposed, what was clicked, and which
// clicks have no exposure to learn from.
func (st *Store) Attribution(ctx context.Context, tenant string, opts AttributionOptions) (AttributionPage, error) {
	var page AttributionPage
	if strings.TrimSpace(tenant) == "" {
		return page, fmt.Errorf("signal: tenant is required")
	}
	if err := opts.Stage.validate(); err != nil {
		return page, err
	}
	if err := opts.Window.Validate(); err != nil {
		return page, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}
	pred, predArgs := opts.Window.predicate("occurred_at")

	var sb strings.Builder
	args := []any{tenant, string(opts.Stage)}
	fmt.Fprintf(&sb, `SELECT render_id, c.1, c.2, c.3, c.4, c.5, c.6, c.7, c.8, c.9, c.10
FROM (
    SELECT render_id,
        argMax(tuple(query_id, surface, ranker, language, subject_kind, subject, entity_types, entity_ids, positions, occurred_at), version) AS c
    FROM %s.exposures
    WHERE tenant = ? AND stage = ?%s`, st.db, pred)
	args = append(args, predArgs...)
	if surface := strings.TrimSpace(opts.Surface); surface != "" {
		sb.WriteString(" AND surface = ?")
		args = append(args, surface)
	}
	if opts.After != "" {
		sb.WriteString(" AND render_id > ?")
		args = append(args, opts.After)
	}
	sb.WriteString("\n    GROUP BY render_id\n)\nORDER BY render_id LIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return page, fmt.Errorf("signal: attribution exposures: %w", err)
	}
	byRender := map[string]int{}
	for rows.Next() {
		var (
			r          AttributedRender
			kind, key  string
			types, ids []string
			positions  []uint32
		)
		if err := rows.Scan(&r.RenderID, &r.QueryID, &r.Surface, &r.Ranker, &r.Language, &kind, &key, &types, &ids, &positions, &r.OccurredAt); err != nil {
			rows.Close()
			return page, fmt.Errorf("signal: attribution scan: %w", err)
		}
		if key != "" {
			r.Subject = subjectFromKey(kind, key)
		}
		for i := range ids {
			r.Shown = append(r.Shown, Placement{EntityRef: EntityRef{EntityType: types[i], EntityID: ids[i]}, Position: positions[i]})
		}
		byRender[r.RenderID] = len(page.Renders)
		page.Renders = append(page.Renders, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Renders) == limit {
		page.Next = page.Renders[len(page.Renders)-1].RenderID
	}

	// Canonical clicks of the window; those for this page's renders join, the
	// rest (first page only) are reported as unattributed.
	var cb strings.Builder
	cargs := []any{tenant}
	fmt.Fprintf(&cb, `SELECT render_id, entity_type, entity_id, event_id, c.1, c.2
FROM (
    SELECT JSONExtractString(argMax(payload, version), '%s') AS render_id, entity_type, entity_id, event_id,
        argMax(tuple(occurred_at, toUInt32(JSONExtractUInt(payload, '%s'))), version) AS c
    FROM %s.events
    WHERE tenant = ? AND signal_type = 'click'%s
    GROUP BY entity_type, entity_id, subject_kind, subject, event_id
)
WHERE render_id != ''`, PayloadKeyRenderID, PayloadKeyPosition, st.db, pred)
	cargs = append(cargs, predArgs...)
	if opts.After != "" || len(page.Renders) == limit {
		renderIDs := make([]string, 0, len(page.Renders))
		for _, r := range page.Renders {
			renderIDs = append(renderIDs, r.RenderID)
		}
		if len(renderIDs) == 0 {
			return page, nil
		}
		cb.WriteString(" AND render_id IN ?")
		cargs = append(cargs, renderIDs)
	} else {
		var eb strings.Builder
		fmt.Fprintf(&eb, "SELECT DISTINCT render_id FROM %s.exposures WHERE tenant = ? AND stage = ?%s", st.db, pred)
		eargs := append([]any{tenant, string(opts.Stage)}, predArgs...)
		if surface := strings.TrimSpace(opts.Surface); surface != "" {
			eb.WriteString(" AND surface = ?")
			eargs = append(eargs, surface)
		}
		cb.WriteString(" AND (render_id IN ? OR render_id NOT IN (" + eb.String() + "))")
		renderIDs := make([]string, 0, len(page.Renders))
		for _, r := range page.Renders {
			renderIDs = append(renderIDs, r.RenderID)
		}
		cargs = append(cargs, renderIDs)
		cargs = append(cargs, eargs...)
	}
	cb.WriteString("\nORDER BY render_id, c.1, entity_type, entity_id, event_id")
	crows, err := st.conn.Query(ctx, cb.String(), cargs...)
	if err != nil {
		return page, fmt.Errorf("signal: attribution clicks: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var (
			renderID string
			click    AttributedClick
		)
		if err := crows.Scan(&renderID, &click.EntityType, &click.EntityID, &click.EventID, &click.OccurredAt, &click.Position); err != nil {
			return page, fmt.Errorf("signal: attribution click scan: %w", err)
		}
		idx, ok := byRender[renderID]
		if !ok {
			page.Unattributed = append(page.Unattributed, click)
			continue
		}
		for _, p := range page.Renders[idx].Shown {
			if p.EntityRef == click.EntityRef {
				click.Exposed = true
				break
			}
		}
		page.Renders[idx].Clicks = append(page.Renders[idx].Clicks, click)
	}
	return page, crows.Err()
}
