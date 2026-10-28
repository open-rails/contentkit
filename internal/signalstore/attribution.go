package signalstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-rails/contentkit/contentref"
	signal "github.com/open-rails/contentkit/signal"
)

// Attribution joins canonical clicks to canonical exposures at one stage, so
// evaluation sees what was actually exposed, what was clicked, and which
// clicks have no exposure to learn from.
//
// The export is one deterministic sequence: renders in render_id order, each
// with its clicks in (occurred_at, content, subject, event_id) order, followed
// by the unattributed clicks in (render_id, occurred_at, content, subject,
// event_id) order. Every page is bounded by Limit renders and ClickLimit click
// rows; Next resumes exactly after the last row emitted, inside a render when
// its clicks did not fit. Concatenating all pages at any size yields the same
// rows once each. Erased subjects are excluded at read time.
func (st *Store) Attribution(ctx context.Context, tenant string, opts signal.AttributionOptions) (signal.AttributionPage, error) {
	var page signal.AttributionPage
	if strings.TrimSpace(tenant) == "" {
		return page, fmt.Errorf("signal: tenant is required")
	}
	if err := validateStage(opts.Stage); err != nil {
		return page, err
	}
	if err := opts.Window.Validate(); err != nil {
		return page, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = signal.DefaultAttributionRenders
	}
	budget := opts.ClickLimit
	if budget <= 0 {
		budget = signal.DefaultAttributionClicks
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
			return page, st.attributeVisits(ctx, tenant, &page)
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
	return page, st.attributeVisits(ctx, tenant, &page)
}

// attributionRenders fills the renders phase of one page and returns the
// cursor to continue it, or nil when the phase is exhausted (budget then holds
// the click rows left for the unattributed stream).
func (st *Store) attributionRenders(ctx context.Context, page *signal.AttributionPage, tenant string, opts signal.AttributionOptions, cur attributionCursor, limit int, budget *int) (*attributionCursor, error) {
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
			if p.Equal(c.click.ContentRef) {
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
func (st *Store) attributionUnattributed(ctx context.Context, page *signal.AttributionPage, tenant string, opts signal.AttributionOptions, cur attributionCursor, budget int) (*attributionCursor, error) {
	canon, args := st.canonicalClicks(tenant, opts)
	known, knownArgs := st.exposurePredicate(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nFROM (%s)\nWHERE render_id != '' AND render_id NOT IN (SELECT DISTINCT render_id FROM %s.exposures WHERE %s)",
		clickSelect, canon, st.db, known)
	args = append(args, knownArgs...)
	if cur.Click != nil {
		sb.WriteString(" AND (render_id, " + clickKeyColumns + ") > (?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, cur.Click.Render)
		args = append(args, cur.Click.args()...)
	}
	sb.WriteString("\nORDER BY render_id, " + clickKeyColumns + "\nLIMIT ?")
	args = append(args, budget+1)
	rows, err := st.scanClicks(ctx, tenant, sb.String(), args...)
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
func (st *Store) stageRenders(ctx context.Context, tenant string, opts signal.AttributionOptions, cur attributionCursor, limit int) ([]signal.AttributedRender, bool, error) {
	pred, args := st.exposurePredicate(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, `SELECT render_id, c.1, c.2, c.3, c.4, c.5, c.6, c.7, c.8, c.9, c.10, c.11, c.12, c.13
FROM (
    SELECT render_id,
        argMax(tuple(query_id, surface, ranker, language, subject_kind, subject, content_kinds, content_ids, content_version_ids, positions, occurred_at, access_contexts, search_query), version) AS c
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
	var out []signal.AttributedRender
	for rows.Next() {
		var (
			r                    signal.AttributedRender
			kind, key            string
			kinds, ids, versions []string
			positions            []uint32
			contexts             []string
		)
		if err := rows.Scan(&r.RenderID, &r.QueryID, &r.Surface, &r.Ranker, &r.Language, &kind, &key, &kinds, &ids, &versions, &positions, &r.OccurredAt, &contexts, &r.SearchQuery); err != nil {
			return nil, false, fmt.Errorf("signal: attribution scan: %w", err)
		}
		if key != "" {
			r.Subject = subjectFromKey(kind, key)
		}
		for i := range ids {
			version := ""
			if i < len(versions) {
				version = versions[i]
			}
			p := signal.Placement{ContentRef: contentref.NewVersion(tenant, kinds[i], ids[i], version), Position: positions[i]}
			if i < len(contexts) && contexts[i] != "" {
				if err := json.Unmarshal([]byte(contexts[i]), &p.Access); err != nil {
					return nil, false, err
				}
			}
			r.Shown = append(r.Shown, p)
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
func (st *Store) renderClicks(ctx context.Context, tenant string, opts signal.AttributionOptions, ids []string, cur attributionCursor, budget int) ([]clickRow, string, error) {
	canon, args := st.canonicalClicks(tenant, opts)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nFROM (%s)\nWHERE render_id IN ?", clickSelect, canon)
	args = append(args, ids)
	if cur.Partial && cur.Click != nil {
		sb.WriteString(" AND (render_id > ? OR (" + clickKeyColumns + ") > (?, ?, ?, ?, ?, ?, ?))")
		args = append(args, cur.Render)
		args = append(args, cur.Click.args()...)
	}
	sb.WriteString("\nORDER BY render_id, " + clickKeyColumns + "\nLIMIT ?")
	args = append(args, budget+1)
	rows, err := st.scanClicks(ctx, tenant, sb.String(), args...)
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
func (st *Store) canonicalClicks(tenant string, opts signal.AttributionOptions) (string, []any) {
	pred, predArgs := windowPredicate(opts.Window, "occurred_at")
	q := fmt.Sprintf(`SELECT JSONExtractString(argMax(payload, version), '%[1]s') AS render_id,
        %[6]s, subject_kind, subject, event_id,
        argMax(tuple(occurred_at, toUInt32(JSONExtractUInt(payload, '%[2]s'))), version) AS c
    FROM %[3]s.signals
    WHERE tenant = ? AND signal_type = ?%[4]s AND %[5]s
    GROUP BY %[6]s, subject_kind, subject, event_id`,
		signal.PayloadKeyRenderID, signal.PayloadKeyPosition, st.db, pred, st.notErased(), refColumns)
	args := append([]any{tenant, signal.TypeClick}, predArgs...)
	return q, args
}

// exposurePredicate renders the WHERE of the export's exposure rows: tenant,
// stage, window, surface and the erasure ledger.
func (st *Store) exposurePredicate(tenant string, opts signal.AttributionOptions) (string, []any) {
	pred, predArgs := windowPredicate(opts.Window, "occurred_at")
	sb := "tenant = ? AND stage = ?" + pred
	args := append([]any{tenant, string(opts.Stage)}, predArgs...)
	if surface := strings.TrimSpace(opts.Surface); surface != "" {
		sb += " AND surface = ?"
		args = append(args, surface)
	}
	return sb + " AND " + st.notErased(), args
}

const (
	clickSelect = "SELECT render_id, " + refColumns + ", subject_kind, subject, event_id, c.1, c.2"
)

const (
	clickKeyColumns = "c.1, " + refColumns + ", subject_kind, subject, event_id"
)

type clickRow struct {
	key   clickKey
	click signal.AttributedClick
}

func (st *Store) scanClicks(ctx context.Context, tenant, q string, args ...any) ([]clickRow, error) {
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clickRow
	for rows.Next() {
		r, err := scanClick(rows, tenant)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanClick(rows driver.Rows, tenant string) (clickRow, error) {
	var r clickRow
	k := &r.key
	if err := rows.Scan(&k.Render, &k.ContentKind, &k.ContentID, &k.ContentVersionID, &k.SubjectKind, &k.Subject, &k.EventID, &k.OccurredAt, &r.click.Position); err != nil {
		return r, err
	}
	r.click.ContentRef = contentref.NewVersion(tenant, k.ContentKind, k.ContentID, k.ContentVersionID)
	r.click.Subject = subjectFromKey(k.SubjectKind, k.Subject)
	r.click.EventID = k.EventID
	r.click.OccurredAt = k.OccurredAt
	return r, nil
}

// clickKey is the total export order of canonical clicks within a render:
// (occurred_at, content_kind, content_id, content_version_id, subject_kind,
// subject, event_id); the last six columns are the click's identity, so the
// key is unique.
type clickKey struct {
	Render           string    `json:"r"`
	OccurredAt       time.Time `json:"t"`
	ContentKind      string    `json:"ck"`
	ContentID        string    `json:"ci"`
	ContentVersionID string    `json:"cv,omitempty"`
	SubjectKind      string    `json:"sk"`
	Subject          string    `json:"s"`
	EventID          string    `json:"e"`
}

func (k clickKey) args() []any {
	return []any{k.OccurredAt.UTC(), k.ContentKind, k.ContentID, k.ContentVersionID, k.SubjectKind, k.Subject, k.EventID}
}

const (
	phaseRenders = "r"
)

const (
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

func attributionScope(opts signal.AttributionOptions) string {
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
