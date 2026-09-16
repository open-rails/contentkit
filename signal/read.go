package signal

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

const stateColumns = `entity_type, entity_id, first_seen_at, last_signal_at, total_events, views, completions,
 active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback`

func scanStateRow(rows driver.Rows) (StateRow, error) {
	var r StateRow
	if err := rows.Scan(
		&r.EntityType, &r.EntityID, &r.FirstSeenAt, &r.LastSignalAt, &r.TotalEvents, &r.Views, &r.Completions,
		&r.ActiveS, &r.MaxProgress, &r.ProgressMax, &r.Completed, &r.Resume, &r.LastScore, &r.NetValue, &r.Feedback,
	); err != nil {
		return StateRow{}, err
	}
	r.Seen = r.MaxProgress > 0
	return r, nil
}

// States is the bulk "annotate this list with view context" read: for each
// requested entity, the subject's standing (seen?, progress bar, completed?,
// resume pointer). Entities the subject has no signals for are absent.
func (st *Store) States(ctx context.Context, tenant string, subject Subject, refs []EntityRef) (map[EntityRef]State, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	out := make(map[EntityRef]State, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	byType := map[string][]string{}
	for _, r := range refs {
		if err := r.validate(); err != nil {
			return nil, err
		}
		byType[r.EntityType] = append(byType[r.EntityType], r.EntityID)
	}
	clauses := make([]string, 0, len(byType))
	args := []any{tenant, subject.Kind(), subject.Key()}
	for _, t := range sortedKeys(byType) {
		clauses = append(clauses, "(entity_type = ? AND entity_id IN ?)")
		args = append(args, t, byType[t])
	}
	q := fmt.Sprintf(`SELECT %s
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND (%s)`,
		stateColumns, st.db, strings.Join(clauses, " OR "))
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanStateRow(rows)
		if err != nil {
			return nil, fmt.Errorf("signal: states scan: %w", err)
		}
		out[r.EntityRef] = r.State
	}
	return out, rows.Err()
}

func historyFilter(sb *strings.Builder, args []any, opts HistoryOptions) ([]any, error) {
	if t := strings.TrimSpace(opts.EntityType); t != "" {
		sb.WriteString(" AND entity_type = ?")
		args = append(args, t)
	}
	switch opts.Status {
	case HistoryAny:
	case HistorySeen:
		sb.WriteString(" AND max_progress > 0")
	case HistoryInProgress:
		sb.WriteString(" AND max_progress > 0 AND NOT completed")
	case HistoryCompleted:
		sb.WriteString(" AND completed")
	default:
		return nil, fmt.Errorf("signal: invalid HistoryOptions.Status %q", opts.Status)
	}
	if !opts.Since.IsZero() {
		sb.WriteString(" AND last_signal_at >= ?")
		args = append(args, opts.Since.UTC())
	}
	return args, nil
}

// History returns the subject's state rows, most recent signal first.
func (st *Store) History(ctx context.Context, tenant string, subject Subject, opts HistoryOptions) ([]StateRow, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `SELECT %s
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ?`, stateColumns, st.db)
	args, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key()}, opts)
	if err != nil {
		return nil, err
	}
	sb.WriteString(" ORDER BY last_signal_at DESC, entity_type ASC, entity_id ASC LIMIT ? OFFSET ?")
	args = append(args, limit, opts.Offset)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: history: %w", err)
	}
	defer rows.Close()
	out := make([]StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows)
		if err != nil {
			return nil, fmt.Errorf("signal: history scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HistoryCount returns the total row count History would paginate over.
func (st *Store) HistoryCount(ctx context.Context, tenant string, subject Subject, opts HistoryOptions) (int64, error) {
	if err := subject.Validate(); err != nil {
		return 0, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `SELECT toInt64(count())
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ?`, st.db)
	args, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key()}, opts)
	if err != nil {
		return 0, err
	}
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("signal: history count: %w", err)
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("signal: history count scan: %w", err)
		}
	}
	return n, rows.Err()
}

// SeenIDs returns the subject's seen-set for one entity type: entity ids with
// max_progress > 0. This is the signal-plane half of the unseen anti-join.
func (st *Store) SeenIDs(ctx context.Context, tenant string, subject Subject, entityType string) (map[string]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(entityType) == "" {
		return nil, fmt.Errorf("signal: entityType is required")
	}
	q := fmt.Sprintf(`SELECT entity_id
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND entity_type = ? AND max_progress > 0`, st.db)
	rows, err := st.conn.Query(ctx, q, tenant, subject.Kind(), subject.Key(), entityType)
	if err != nil {
		return nil, fmt.Errorf("signal: seen ids: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("signal: seen ids scan: %w", err)
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// NegativeIDs returns entities the subject has net-negative canonical feedback
// for: the exclusion set for recommendations.
func (st *Store) NegativeIDs(ctx context.Context, tenant string, subject Subject, entityTypes []string) (map[EntityRef]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	var sb strings.Builder
	args := []any{tenant, subject.Kind(), subject.Key()}
	fmt.Fprintf(&sb, `SELECT entity_type, entity_id
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND net_value < 0`, st.db)
	if types := trimAll(entityTypes); len(types) > 0 {
		sb.WriteString(" AND entity_type IN ?")
		args = append(args, types)
	}
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: negative ids: %w", err)
	}
	defer rows.Close()
	out := map[EntityRef]struct{}{}
	for rows.Next() {
		var ref EntityRef
		if err := rows.Scan(&ref.EntityType, &ref.EntityID); err != nil {
			return nil, fmt.Errorf("signal: negative ids scan: %w", err)
		}
		out[ref] = struct{}{}
	}
	return out, rows.Err()
}

// TopStates returns the subject's highest-signal entities (recommendation
// seeds): ordered by last_score DESC, then recency.
func (st *Store) TopStates(ctx context.Context, tenant string, subject Subject, opts TopStatesOptions) ([]StateRow, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	var sb strings.Builder
	args := []any{tenant, subject.Kind(), subject.Key()}
	fmt.Fprintf(&sb, `SELECT %s
FROM %s.subject_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ?`, stateColumns, st.db)
	if types := trimAll(opts.EntityTypes); len(types) > 0 {
		sb.WriteString(" AND entity_type IN ?")
		args = append(args, types)
	}
	if opts.ExcludeNegative {
		sb.WriteString(" AND net_value >= 0")
	}
	sb.WriteString(" ORDER BY last_score DESC, last_signal_at DESC, entity_type ASC, entity_id ASC LIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: top states: %w", err)
	}
	defer rows.Close()
	out := make([]StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows)
		if err != nil {
			return nil, fmt.Errorf("signal: top states scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// metricColumns are the window metric aliases (also usable in RankExpr).
const metricColumns = `viewers, user_viewers, anon_viewers, views, completions, completers, active_s,
 score_sum, events, value_sum, positive_subjects, negative_subjects, signal_counts`

// windowMetrics aggregates subject_daily for one entity type over a window:
// first per subject (so each subject counts once), then per entity.
func (st *Store) windowMetrics(tenant, entityType string, ids []string, win Window) (string, []any) {
	var where strings.Builder
	args := []any{tenant, entityType}
	if len(ids) > 0 {
		where.WriteString(" AND entity_id IN ?")
		args = append(args, ids)
	}
	pred, predArgs := win.dayPredicate("day")
	where.WriteString(pred)
	args = append(args, predArgs...)
	return fmt.Sprintf(`SELECT entity_id,
    toUInt64(countIf(s_views > 0)) AS viewers,
    toUInt64(countIf(s_views > 0 AND subject_kind = '%[3]s')) AS user_viewers,
    toUInt64(countIf(s_views > 0 AND subject_kind = '%[4]s')) AS anon_viewers,
    toUInt64(sum(s_views)) AS views,
    toUInt64(sum(s_completions)) AS completions,
    toUInt64(countIf(s_completions > 0)) AS completers,
    toUInt64(sum(s_active)) AS active_s,
    toInt64(sum(s_score)) AS score_sum,
    toUInt64(sum(s_events)) AS events,
    toFloat64(sum(s_value)) AS value_sum,
    toUInt64(countIf(s_value > 0)) AS positive_subjects,
    toUInt64(countIf(s_value < 0)) AS negative_subjects,
    CAST(sumMap(s_types), 'Map(String, UInt64)') AS signal_counts
FROM (
    SELECT entity_id, subject_kind, subject,
        sum(views) AS s_views, sum(completions) AS s_completions, sum(active_s) AS s_active,
        sum(score_sum) AS s_score, sum(events) AS s_events, sum(value_sum) AS s_value,
        sumMap(type_counts) AS s_types
    FROM %[1]s.subject_daily FINAL
    WHERE tenant = ? AND entity_type = ? AND events > 0%[2]s
    GROUP BY entity_id, subject_kind, subject
)
GROUP BY entity_id`, st.db, where.String(), SubjectKindUser, SubjectKindAnon), args
}

func scanMetrics(rows driver.Rows, id *string, m *EntityMetrics, extra ...any) error {
	dest := []any{id, &m.Viewers, &m.UserViewers, &m.AnonViewers, &m.Views, &m.Completions, &m.Completers,
		&m.ActiveS, &m.ScoreSum, &m.Events, &m.ValueSum, &m.PositiveSubjects, &m.NegativeSubjects, &m.SignalCounts}
	return rows.Scan(append(dest, extra...)...)
}

// finalSettings: day is in the sorting key and partition key, so FINAL never
// needs to merge across partitions.
const finalSettings = "\nSETTINGS do_not_merge_across_partitions_select_final = 1"

// Metrics returns named window metrics for entity ids of one type (zero window
// = all time). Ids with no canonical events in the window are absent.
func (st *Store) Metrics(ctx context.Context, tenant string, entityType string, ids []string, window Window) (map[string]EntityMetrics, error) {
	out := map[string]EntityMetrics{}
	ids = trimAll(ids)
	if strings.TrimSpace(entityType) == "" {
		return nil, fmt.Errorf("signal: entityType is required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	q, args := st.windowMetrics(tenant, entityType, ids, window)
	rows, err := st.conn.Query(ctx, q+finalSettings, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id string
			m  EntityMetrics
		)
		if err := scanMetrics(rows, &id, &m); err != nil {
			return nil, fmt.Errorf("signal: metrics scan: %w", err)
		}
		out[id] = m
	}
	return out, rows.Err()
}

// Popular ranks entities of one type with at least one view in a literal
// window. Every view in the window has equal time weight.
func (st *Store) Popular(ctx context.Context, tenant string, entityType string, opts PopularOptions) ([]PopularHit, error) {
	return st.popular(ctx, tenant, entityType, nil, opts)
}

// PopularityFor scores a fixed candidate set by the popularity ranking and
// returns entity_id -> score. Candidates without views in the window are absent.
func (st *Store) PopularityFor(ctx context.Context, tenant string, entityType string, ids []string, window Window) (map[string]float64, error) {
	ids = trimAll(ids)
	if len(ids) == 0 {
		return map[string]float64{}, nil
	}
	hits, err := st.popular(ctx, tenant, entityType, ids, PopularOptions{Window: window, Limit: len(ids)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(hits))
	for _, h := range hits {
		out[h.EntityID] = h.Score
	}
	return out, nil
}

// defaultRankExpr builds the default ranking over the window metric columns.
func defaultRankExpr(w RankWeights) string {
	return fmt.Sprintf(
		"log10(1 + toFloat64(viewers)) * greatest(%g, (toFloat64(score_sum) + %g) / (toFloat64(views) + %g))",
		w.QualityFloor, w.PriorScore*w.PriorWeight, w.PriorWeight,
	)
}

func (st *Store) popular(ctx context.Context, tenant string, entityType string, ids []string, opts PopularOptions) ([]PopularHit, error) {
	if strings.TrimSpace(entityType) == "" {
		return nil, fmt.Errorf("signal: entityType is required")
	}
	if err := opts.Window.Validate(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	rankExpr := strings.TrimSpace(opts.RankExpr)
	if rankExpr == "" {
		rankExpr = defaultRankExpr(opts.Weights.withDefaults())
	}
	metrics, args := st.windowMetrics(tenant, entityType, ids, opts.Window)
	q := fmt.Sprintf(`SELECT entity_id, %s, (%s) AS rank_score
FROM (%s)
WHERE views > 0
ORDER BY rank_score DESC, entity_id ASC
LIMIT ?%s`, metricColumns, rankExpr, metrics, finalSettings)
	rows, err := st.conn.Query(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("signal: popular: %w", err)
	}
	defer rows.Close()
	out := make([]PopularHit, 0, limit)
	for rows.Next() {
		h := PopularHit{EntityRef: EntityRef{EntityType: entityType}}
		if err := scanMetrics(rows, &h.EntityID, &h.EntityMetrics, &h.Score); err != nil {
			return nil, fmt.Errorf("signal: popular scan: %w", err)
		}
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			h.Score = 0
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CoEngaged returns entities co-engaged with the anchor: "subjects who
// engaged with X also engaged with Y". Strength nets subjects whose summed
// feedback for the candidate is non-negative against those with negative.
func (st *Store) CoEngaged(ctx context.Context, tenant string, ref EntityRef, opts CoEngagedOptions) ([]CoEngagedHit, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	if err := opts.Window.Validate(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	maxSubjects := opts.MaxSubjects
	if maxSubjects <= 0 {
		maxSubjects = 10000
	}
	if !opts.SkipRollup {
		hits, err := st.coEngagedFromRollup(ctx, tenant, ref, opts, limit)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			return hits, nil
		}
	}
	pred, predArgs := opts.Window.dayPredicate("day")
	args := []any{tenant}
	var candidates strings.Builder
	if types := trimAll(opts.EntityTypes); len(types) > 0 {
		candidates.WriteString(" AND entity_type IN ?")
		args = append(args, types)
	}
	args = append(args, predArgs...)
	args = append(args, ref.EntityType, ref.EntityID, tenant, ref.EntityType, ref.EntityID)
	args = append(args, predArgs...)
	args = append(args, maxSubjects, limit)
	q := fmt.Sprintf(`SELECT entity_type, entity_id,
    toInt64(countIf(net >= 0)) - toInt64(countIf(net < 0)) AS strength
FROM (
    SELECT entity_type, entity_id, subject_kind, subject, sum(value_sum) AS net
    FROM %[1]s.subject_daily FINAL
    WHERE tenant = ? AND events > 0%[2]s%[3]s
      AND NOT (entity_type = ? AND entity_id = ?)
      AND (subject_kind, subject) IN (
          SELECT DISTINCT subject_kind, subject FROM %[1]s.subject_daily FINAL
          WHERE tenant = ? AND entity_type = ? AND entity_id = ? AND events > 0%[3]s
          LIMIT ?)
    GROUP BY entity_type, entity_id, subject_kind, subject
)
GROUP BY entity_type, entity_id
HAVING strength > 0
ORDER BY strength DESC, entity_type ASC, entity_id ASC
LIMIT ?%[4]s`, st.db, candidates.String(), pred, finalSettings)
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged: %w", err)
	}
	defer rows.Close()
	out := make([]CoEngagedHit, 0, limit)
	for rows.Next() {
		var h CoEngagedHit
		if err := rows.Scan(&h.EntityType, &h.EntityID, &h.Strength); err != nil {
			return nil, fmt.Errorf("signal: co-engaged scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// coEngagedFromRollup serves CoEngaged from the precomputed item_pairs table.
func (st *Store) coEngagedFromRollup(ctx context.Context, tenant string, ref EntityRef, opts CoEngagedOptions, limit int) ([]CoEngagedHit, error) {
	var sb strings.Builder
	args := []any{tenant, ref.EntityType, ref.EntityID}
	fmt.Fprintf(&sb, `SELECT entity_type_b, entity_id_b, max(strength) AS s
FROM %s.item_pairs
WHERE tenant = ? AND entity_type_a = ? AND entity_id_a = ?`, st.db)
	if types := trimAll(opts.EntityTypes); len(types) > 0 {
		sb.WriteString(" AND entity_type_b IN ?")
		args = append(args, types)
	}
	sb.WriteString("\nGROUP BY entity_type_b, entity_id_b\nHAVING s > 0\nORDER BY s DESC, entity_type_b ASC, entity_id_b ASC\nLIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged rollup: %w", err)
	}
	defer rows.Close()
	out := make([]CoEngagedHit, 0, limit)
	for rows.Next() {
		var h CoEngagedHit
		if err := rows.Scan(&h.EntityType, &h.EntityID, &h.Strength); err != nil {
			return nil, fmt.Errorf("signal: co-engaged rollup scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RefreshCoEngagement (re)materializes the item_pairs rollup for one tenant:
// per subject, the distinct entities with non-negative summed feedback
// (capped at MaxEntitiesPerSubject) are cross-joined into pairs; strength =
// co-engaged subjects minus subjects with negative feedback on the candidate.
func (st *Store) RefreshCoEngagement(ctx context.Context, tenant string, opts RefreshCoEngagementOptions) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := opts.Window.Validate(); err != nil {
		return err
	}
	maxPer := opts.MaxEntitiesPerSubject
	if maxPer <= 0 {
		maxPer = 100
	}
	if err := st.conn.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.item_pairs WHERE tenant = ?`, st.db), tenant); err != nil {
		return fmt.Errorf("signal: clear item_pairs: %w", err)
	}
	winPred, winArgs := opts.Window.dayPredicate("day")
	q := fmt.Sprintf(`INSERT INTO %[1]s.item_pairs
(tenant, entity_type_a, entity_id_a, entity_type_b, entity_id_b, strength, refreshed_at)
SELECT
    '%[2]s' AS tenant,
    a.1 AS entity_type_a, a.2 AS entity_id_a,
    (bs.1).1 AS entity_type_b, (bs.1).2 AS entity_id_b,
    toInt64(sum(bs.2)) AS strength,
    now()
FROM (
    SELECT arrayJoin(pos) AS a, arrayJoin(bsigned) AS bs
    FROM (
        SELECT
            pos,
            arrayConcat(arrayMap(x -> (x, 1), pos), arrayMap(x -> (x, -1), neg)) AS bsigned
        FROM (
            SELECT
                subject_kind, subject,
                groupUniqArrayIf(%[3]d)((entity_type, entity_id), net_v >= 0) AS pos,
                groupUniqArrayIf(%[3]d)((entity_type, entity_id), net_v < 0) AS neg
            FROM (
                SELECT subject_kind, subject, entity_type, entity_id, sum(value_sum) AS net_v
                FROM %[1]s.subject_daily FINAL
                WHERE tenant = ? AND events > 0%[4]s
                GROUP BY subject_kind, subject, entity_type, entity_id
            )
            GROUP BY subject_kind, subject
        )
    )
)
WHERE a != bs.1
GROUP BY entity_type_a, entity_id_a, entity_type_b, entity_id_b
HAVING strength > 0%[5]s`, st.db, escapeCHString(tenant), maxPer, winPred, finalSettings)
	if err := st.conn.Exec(ctx, q, append([]any{tenant}, winArgs...)...); err != nil {
		return fmt.Errorf("signal: refresh co-engagement: %w", err)
	}
	return nil
}
