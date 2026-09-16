package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TypeClick is the selection signal type Attribution joins to exposures.
const TypeClick = "click"

// Default page bounds of Attribution.
const (
	DefaultAttributionRenders = 500
	DefaultAttributionClicks  = 5000
)

// AttributionOptions selects an evaluation export. Stage is required. A paged
// export passes the previous page's Next as After with the same Window, Stage
// and Surface; Limit and ClickLimit may change between pages.
type AttributionOptions struct {
	// Window bounds exposure and click days (zero = all time).
	Window Window
	// Stage is the exposure stage clicks are attributed against (required).
	Stage ExposureStage
	// Surface restricts renders to one surface (empty = all).
	Surface string
	// After is the opaque cursor from the previous page's Next (empty = start).
	After string
	// Limit caps renders per page (default DefaultAttributionRenders).
	Limit int
	// ClickLimit caps click rows per page, attributed and unattributed
	// together (default DefaultAttributionClicks). A render with more clicks
	// than fit continues on the next page.
	ClickLimit int
}

// AttributedClick is a canonical click carrying a render id.
type AttributedClick struct {
	EntityRef
	Subject    Subject // who clicked; may differ from the render's subject
	Position   uint32  // position claimed by the click
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
	// Continued is true when earlier clicks of this render were on the previous
	// page: the header repeats, the clicks do not. Merge by RenderID.
	Continued bool
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
//
// The export is one deterministic sequence: renders in render_id order, each
// with its clicks in (occurred_at, entity, subject, event_id) order, followed
// by the unattributed clicks in (render_id, occurred_at, entity, subject,
// event_id) order. Every page is bounded by Limit renders and ClickLimit click
// rows; Next resumes exactly after the last row emitted, inside a render when
// its clicks did not fit. Concatenating all pages at any size yields the same
// rows once each. Erased subjects are excluded at read time.
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
		limit = DefaultAttributionRenders
	}
	budget := opts.ClickLimit
	if budget <= 0 {
		budget = DefaultAttributionClicks
	}
	scope := attributionScope(opts)
	cur := attributionCursor{Scope: scope, Phase: phaseRenders}
	if opts.After != "" {
		var err error
		if cur, err = decodeAttributionCursor(opts.After, scope); err != nil {
			return page, err
		}
	}
	if cur.Phase == phaseRenders {
		next, err := st.attributionRenders(ctx, &page, tenant, opts, cur, limit, &budget)
		if err != nil {
			return page, err
		}
		if next != nil {
			page.Next = next.encode()
			return page, nil
		}
		cur = attributionCursor{Scope: scope, Phase: phaseUnattributed}
	}
	next, err := st.attributionUnattributed(ctx, &page, tenant, opts, cur, budget)
	if err != nil {
		return page, err
	}
	if next != nil {
		page.Next = next.encode()
	}
	return page, nil
}

// attributionRenders fills the renders phase of one page and returns the
// cursor to continue it, or nil when the phase is exhausted (budget then holds
// the click rows left for the unattributed stream).
func (st *Store) attributionRenders(ctx context.Context, page *AttributionPage, tenant string, opts AttributionOptions, cur attributionCursor, limit int, budget *int) (*attributionCursor, error) {
	renders, more, err := st.stageRenders(ctx, tenant, opts, cur, limit)
	if err != nil {
		return nil, err
	}
	if len(renders) == 0 {
		return nil, nil
	}
	byRender := make(map[string]int, len(renders))
	ids := make([]string, len(renders))
	for i := range renders {
		byRender[renders[i].RenderID] = i
		ids[i] = renders[i].RenderID
	}
	if cur.Partial && renders[0].RenderID == cur.Render {
		renders[0].Continued = true
	}
	clicks, sentinel, err := st.renderClicks(ctx, tenant, opts, ids, cur, *budget)
	if err != nil {
		return nil, err
	}
	for _, c := range clicks {
		r := &renders[byRender[c.key.Render]]
		for _, p := range r.Shown {
			if p.EntityRef == c.click.EntityRef {
				c.click.Exposed = true
				break
			}
		}
		r.Clicks = append(r.Clicks, c.click)
	}
	*budget -= len(clicks)
	next := attributionCursor{Scope: cur.Scope, Phase: phaseRenders}
	switch {
	case sentinel != "":
		// Click budget hit: the sentinel is the first click not emitted.
		last := clicks[len(clicks)-1].key
		if sentinel == last.Render {
			page.Renders = renders[:byRender[last.Render]+1]
			next.Render, next.Partial, next.Click = last.Render, true, &last
		} else {
			cut := byRender[sentinel]
			page.Renders = renders[:cut]
			next.Render = renders[cut-1].RenderID
		}
		return &next, nil
	case more:
		page.Renders = renders
		next.Render = renders[len(renders)-1].RenderID
		return &next, nil
	default:
		page.Renders = renders
		return nil, nil
	}
}

// attributionUnattributed fills the unattributed stream with what is left of
// the click budget and returns the cursor to continue it, or nil when done.
func (st *Store) attributionUnattributed(ctx context.Context, page *AttributionPage, tenant string, opts AttributionOptions, cur attributionCursor, budget int) (*attributionCursor, error) {
	canon, args := st.canonicalClicks(tenant, opts)
	known, knownArgs := st.exposurePredicate(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nFROM (%s)\nWHERE render_id != '' AND render_id NOT IN (SELECT DISTINCT render_id FROM %s.exposures WHERE %s)",
		clickSelect, canon, st.db, known)
	args = append(args, knownArgs...)
	if cur.Click != nil {
		sb.WriteString(" AND (render_id, " + clickKeyColumns + ") > (?, ?, ?, ?, ?, ?, ?)")
		args = append(args, cur.Click.Render)
		args = append(args, cur.Click.args()...)
	}
	sb.WriteString("\nORDER BY render_id, " + clickKeyColumns + "\nLIMIT ?")
	args = append(args, budget+1)
	rows, err := st.scanClicks(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: attribution unattributed clicks: %w", err)
	}
	more := len(rows) > budget
	if more {
		rows = rows[:budget]
	}
	for _, r := range rows {
		page.Unattributed = append(page.Unattributed, r.click)
	}
	if !more {
		return nil, nil
	}
	next := attributionCursor{Scope: cur.Scope, Phase: phaseUnattributed, Click: cur.Click}
	if len(rows) > 0 {
		last := rows[len(rows)-1].key
		next.Click = &last
	}
	return &next, nil
}

// stageRenders reads up to limit canonical renders in render_id order from the
// cursor; more reports whether another render follows.
func (st *Store) stageRenders(ctx context.Context, tenant string, opts AttributionOptions, cur attributionCursor, limit int) ([]AttributedRender, bool, error) {
	pred, args := st.exposurePredicate(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, `SELECT render_id, c.1, c.2, c.3, c.4, c.5, c.6, c.7, c.8, c.9, c.10
FROM (
    SELECT render_id,
        argMax(tuple(query_id, surface, ranker, language, subject_kind, subject, entity_types, entity_ids, positions, occurred_at), version) AS c
    FROM %s.exposures
    WHERE %s`, st.db, pred)
	if cur.Render != "" {
		if cur.Partial {
			sb.WriteString(" AND render_id >= ?")
		} else {
			sb.WriteString(" AND render_id > ?")
		}
		args = append(args, cur.Render)
	}
	sb.WriteString("\n    GROUP BY render_id\n)\nORDER BY render_id LIMIT ?")
	args = append(args, limit+1)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, false, fmt.Errorf("signal: attribution exposures: %w", err)
	}
	defer rows.Close()
	var out []AttributedRender
	for rows.Next() {
		var (
			r          AttributedRender
			kind, key  string
			types, ids []string
			positions  []uint32
		)
		if err := rows.Scan(&r.RenderID, &r.QueryID, &r.Surface, &r.Ranker, &r.Language, &kind, &key, &types, &ids, &positions, &r.OccurredAt); err != nil {
			return nil, false, fmt.Errorf("signal: attribution scan: %w", err)
		}
		if key != "" {
			r.Subject = subjectFromKey(kind, key)
		}
		for i := range ids {
			r.Shown = append(r.Shown, Placement{EntityRef: EntityRef{EntityType: types[i], EntityID: ids[i]}, Position: positions[i]})
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// renderClicks reads up to budget canonical clicks of the page's renders in
// export order, resuming after the cursor's click inside a partial render. The
// sentinel is the render id of the first click that did not fit ("" if all fit).
func (st *Store) renderClicks(ctx context.Context, tenant string, opts AttributionOptions, ids []string, cur attributionCursor, budget int) ([]clickRow, string, error) {
	canon, args := st.canonicalClicks(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nFROM (%s)\nWHERE render_id IN ?", clickSelect, canon)
	args = append(args, ids)
	if cur.Partial && cur.Click != nil {
		sb.WriteString(" AND (render_id > ? OR (" + clickKeyColumns + ") > (?, ?, ?, ?, ?, ?))")
		args = append(args, cur.Render)
		args = append(args, cur.Click.args()...)
	}
	sb.WriteString("\nORDER BY render_id, " + clickKeyColumns + "\nLIMIT ?")
	args = append(args, budget+1)
	rows, err := st.scanClicks(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("signal: attribution clicks: %w", err)
	}
	if len(rows) > budget {
		return rows[:budget], rows[budget].key.Render, nil
	}
	return rows, "", nil
}

// canonicalClicks selects, per logical click of the window, the highest-version
// row's render id and c = (occurred_at, position). Superseded and duplicate
// rows never count, whether or not ClickHouse has merged them.
func (st *Store) canonicalClicks(tenant string, opts AttributionOptions) (string, []any) {
	pred, predArgs := opts.Window.predicate("occurred_at")
	q := fmt.Sprintf(`SELECT JSONExtractString(argMax(payload, version), '%s') AS render_id,
        entity_type, entity_id, subject_kind, subject, event_id,
        argMax(tuple(occurred_at, toUInt32(JSONExtractUInt(payload, '%s'))), version) AS c
    FROM %s.events
    WHERE tenant = ? AND signal_type = ?%s AND %s
    GROUP BY entity_type, entity_id, subject_kind, subject, event_id`,
		PayloadKeyRenderID, PayloadKeyPosition, st.db, pred, st.notErased())
	args := append([]any{tenant, TypeClick}, predArgs...)
	return q, append(args, tenant)
}

// exposurePredicate renders the WHERE of the export's exposure rows: tenant,
// stage, window, surface and the erasure ledger.
func (st *Store) exposurePredicate(tenant string, opts AttributionOptions) (string, []any) {
	pred, predArgs := opts.Window.predicate("occurred_at")
	sb := "tenant = ? AND stage = ?" + pred
	args := append([]any{tenant, string(opts.Stage)}, predArgs...)
	if surface := strings.TrimSpace(opts.Surface); surface != "" {
		sb += " AND surface = ?"
		args = append(args, surface)
	}
	return sb + " AND " + st.notErased(), append(args, tenant)
}

// notErased excludes subjects the erasure ledger covers (one tenant arg).
func (st *Store) notErased() string {
	return subjectHashExpr + " NOT IN (SELECT subject_hash FROM " + st.db + ".erasures WHERE tenant = ?)"
}

const (
	clickSelect     = "SELECT render_id, entity_type, entity_id, subject_kind, subject, event_id, c.1, c.2"
	clickKeyColumns = "c.1, entity_type, entity_id, subject_kind, subject, event_id"
)

type clickRow struct {
	key   clickKey
	click AttributedClick
}

func (st *Store) scanClicks(ctx context.Context, q string, args ...any) ([]clickRow, error) {
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clickRow
	for rows.Next() {
		r, err := scanClick(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanClick(rows driver.Rows) (clickRow, error) {
	var r clickRow
	k := &r.key
	if err := rows.Scan(&k.Render, &k.EntityType, &k.EntityID, &k.SubjectKind, &k.Subject, &k.EventID, &k.OccurredAt, &r.click.Position); err != nil {
		return r, err
	}
	r.click.EntityRef = EntityRef{EntityType: k.EntityType, EntityID: k.EntityID}
	r.click.Subject = subjectFromKey(k.SubjectKind, k.Subject)
	r.click.EventID = k.EventID
	r.click.OccurredAt = k.OccurredAt
	return r, nil
}

// clickKey is the total export order of canonical clicks within a render:
// (occurred_at, entity_type, entity_id, subject_kind, subject, event_id); the
// last five columns are the click's identity, so the key is unique.
type clickKey struct {
	Render      string    `json:"r"`
	OccurredAt  time.Time `json:"t"`
	EntityType  string    `json:"et"`
	EntityID    string    `json:"ei"`
	SubjectKind string    `json:"sk"`
	Subject     string    `json:"s"`
	EventID     string    `json:"e"`
}

func (k clickKey) args() []any {
	return []any{k.OccurredAt.UTC(), k.EntityType, k.EntityID, k.SubjectKind, k.Subject, k.EventID}
}

const (
	phaseRenders      = "r"
	phaseUnattributed = "u"
)

// attributionCursor is the decoded Next/After token: the export scope it
// belongs to and the position after the last emitted row.
type attributionCursor struct {
	Scope string `json:"q"`
	Phase string `json:"p"`
	// Render is the renders-phase boundary: resume strictly after it, or at it
	// when Partial (its clicks after Click remain).
	Render  string    `json:"r,omitempty"`
	Partial bool      `json:"partial,omitempty"`
	Click   *clickKey `json:"c,omitempty"`
}

func attributionScope(opts AttributionOptions) string {
	return string(opts.Stage) + "|" + strings.TrimSpace(opts.Surface) + "|" + opts.Window.String()
}

func (c attributionCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeAttributionCursor(token, scope string) (attributionCursor, error) {
	var c attributionCursor
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || (c.Phase != phaseRenders && c.Phase != phaseUnattributed) || (c.Partial && c.Click == nil) {
		return c, fmt.Errorf("signal: invalid attribution cursor")
	}
	if c.Scope != scope {
		return c, fmt.Errorf("signal: attribution cursor belongs to another export (%s), want %s", c.Scope, scope)
	}
	return c, nil
}
