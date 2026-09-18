package signal

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/contentkit/contentref"
)

const stateColumns = refColumns + `, first_seen_at, last_signal_at, total_events, views, completions,
 active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback`

// workLevel selects the rows of the work itself, never of a version.
const workLevel = " AND content_version_id = ''"

func scanStateRow(rows driver.Rows, tenant string) (StateRow, error) {
	var (
		r       StateRow
		version string
	)
	if err := rows.Scan(
		&r.ContentKind, &r.ContentID, &version, &r.FirstSeenAt, &r.LastSignalAt, &r.TotalEvents, &r.Views, &r.Completions,
		&r.ActiveS, &r.MaxProgress, &r.ProgressMax, &r.Completed, &r.Resume, &r.LastScore, &r.NetValue, &r.Feedback,
	); err != nil {
		return StateRow{}, err
	}
	r.TenantID = tenant
	r.ContentRef = r.ContentRef.WithVersion(version)
	r.Seen = r.MaxProgress > 0
	return r, nil
}

// refTuples renders "(content_kind, content_id, content_version_id) IN (...)"
// for references already validated against the tenant.
func refTuples(refs []ContentRef) (string, []any) {
	tuples := make([]string, len(refs))
	args := make([]any, 0, 3*len(refs))
	for i, r := range refs {
		tuples[i] = "(?, ?, ?)"
		args = append(args, r.ContentKind, r.ContentID, r.Version())
	}
	return "(" + refColumns + ") IN (" + strings.Join(tuples, ", ") + ")", args
}

// States is the bulk "annotate this list with view context" read: for each
// requested reference (work or version), the subject's standing (seen?,
// progress bar, completed?, resume pointer). References the subject has no
// signals for are absent.
func (st *Store) States(ctx context.Context, tenant string, subject Subject, refs []ContentRef) (map[ContentKey]State, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	out := make(map[ContentKey]State, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	for _, r := range refs {
		if err := checkRef(tenant, r); err != nil {
			return nil, err
		}
	}
	filter, refArgs := refTuples(refs)
	args := append([]any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}, refArgs...)
	q := fmt.Sprintf(`SELECT %s
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND %s`,
		stateColumns, st.db, st.subjectNotErased(), filter)
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanStateRow(rows, tenant)
		if err != nil {
			return nil, fmt.Errorf("signal: states scan: %w", err)
		}
		out[r.Key()] = r.State
	}
	return out, rows.Err()
}

func historyFilter(sb *strings.Builder, args []any, opts HistoryOptions) ([]any, error) {
	if k := strings.TrimSpace(opts.ContentKind); k != "" {
		sb.WriteString(" AND content_kind = ?")
		args = append(args, k)
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

// History returns the subject's work-level state rows, most recent signal first.
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
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.db, st.subjectNotErased(), workLevel)
	args, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}, opts)
	if err != nil {
		return nil, err
	}
	sb.WriteString(" ORDER BY last_signal_at DESC, content_kind ASC, content_id ASC LIMIT ? OFFSET ?")
	args = append(args, limit, opts.Offset)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: history: %w", err)
	}
	defer rows.Close()
	out := make([]StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows, tenant)
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
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, st.db, st.subjectNotErased(), workLevel)
	args, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}, opts)
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

// SeenIDs returns the subject's seen-set for one content kind: work ids with
// max_progress > 0. This is the signal-plane half of the unseen anti-join.
func (st *Store) SeenIDs(ctx context.Context, tenant string, subject Subject, contentKind string) (map[string]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("signal: contentKind is required")
	}
	q := fmt.Sprintf(`SELECT content_id
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND content_kind = ?%s AND max_progress > 0`, st.db, st.subjectNotErased(), workLevel)
	rows, err := st.conn.Query(ctx, q, tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject), contentKind)
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

// NegativeIDs returns the works the subject has net-negative canonical
// feedback for: the exclusion set for recommendations.
func (st *Store) NegativeIDs(ctx context.Context, tenant string, subject Subject, contentKinds []string) (map[ContentKey]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	var sb strings.Builder
	args := []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}
	fmt.Fprintf(&sb, `SELECT content_kind, content_id
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s AND net_value < 0`, st.db, st.subjectNotErased(), workLevel)
	if kinds := trimAll(contentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: negative ids: %w", err)
	}
	defer rows.Close()
	out := map[ContentKey]struct{}{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, fmt.Errorf("signal: negative ids scan: %w", err)
		}
		out[contentref.New(tenant, kind, id).Key()] = struct{}{}
	}
	return out, rows.Err()
}

// TopStates returns the subject's highest-signal works (recommendation
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
	args := []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}
	fmt.Fprintf(&sb, `SELECT %s
FROM %s.subject_content_state FINAL
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.db, st.subjectNotErased(), workLevel)
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	if opts.ExcludeNegative {
		sb.WriteString(" AND net_value >= 0")
	}
	sb.WriteString(" ORDER BY last_score DESC, last_signal_at DESC, content_kind ASC, content_id ASC LIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: top states: %w", err)
	}
	defer rows.Close()
	out := make([]StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows, tenant)
		if err != nil {
			return nil, fmt.Errorf("signal: top states scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// metricColumns are the window metric aliases (also usable in RankExpr).
const metricColumns = `viewers, user_viewers, anon_viewers, views, completions, completers, active_s,
 score_sum, viewer_engagement_sum, returning_viewers, events, value_sum, positive_subjects, negative_subjects, signal_counts`

// windowMetrics aggregates subject_content_daily rows matching filter over a
// window: first per subject (so each subject counts once), then per content
// reference.
func (st *Store) windowMetrics(tenant, filter string, filterArgs []any, win Window) (string, []any) {
	args := append([]any{tenant}, filterArgs...)
	pred, predArgs := win.dayPredicate("day")
	args = append(args, predArgs...)
	return fmt.Sprintf(`SELECT %[6]s,
    toUInt64(countIf(s_views > 0)) AS viewers,
    toUInt64(countIf(s_views > 0 AND subject_kind = '%[3]s')) AS user_viewers,
    toUInt64(countIf(s_views > 0 AND subject_kind = '%[4]s')) AS anon_viewers,
    toUInt64(sum(s_views)) AS views,
    toUInt64(sum(s_completions)) AS completions,
    toUInt64(countIf(s_completions > 0)) AS completers,
    toUInt64(sum(s_active)) AS active_s,
    toInt64(sum(s_score)) AS score_sum,
    sum(if(s_views > 0, greatest(0., least(1., toFloat64(s_score) / (100. * greatest(toFloat64(s_views), 1.)))), 0.)) AS viewer_engagement_sum,
    toUInt64(countIf(s_views > 1)) AS returning_viewers,
    toUInt64(sum(s_events)) AS events,
    toFloat64(sum(s_value)) AS value_sum,
    toUInt64(countIf(s_value > 0)) AS positive_subjects,
    toUInt64(countIf(s_value < 0)) AS negative_subjects,
    CAST(sumMap(s_types), 'Map(String, UInt64)') AS signal_counts
FROM (
    SELECT %[6]s, subject_kind, subject,
        sum(views) AS s_views, sum(completions) AS s_completions, sum(active_s) AS s_active,
        sum(score_sum) AS s_score, sum(events) AS s_events, sum(value_sum) AS s_value,
        sumMap(type_counts) AS s_types
    FROM %[1]s.subject_content_daily FINAL
    WHERE tenant = ? AND %[2]s AND events > 0%[7]s AND %[5]s
    GROUP BY %[6]s, subject_kind, subject
)
GROUP BY %[6]s`, st.db, filter, SubjectKindUser, SubjectKindAnon, st.notErased(), refColumns, pred), args
}

func scanMetrics(rows driver.Rows, kind, id, version *string, m *ContentMetrics, extra ...any) error {
	dest := []any{kind, id, version, &m.Viewers, &m.UserViewers, &m.AnonViewers, &m.Views, &m.Completions, &m.Completers,
		&m.ActiveS, &m.ScoreSum, &m.ViewerEngagementSum, &m.ReturningViewers, &m.Events, &m.ValueSum, &m.PositiveSubjects, &m.NegativeSubjects, &m.SignalCounts}
	return rows.Scan(append(dest, extra...)...)
}

// finalSettings: day is in the sorting key and partition key, so FINAL never
// needs to merge across partitions.
const finalSettings = "\nSETTINGS do_not_merge_across_partitions_select_final = 1"

// Metrics returns named window metrics for the references (works or versions;
// zero window = all time). References with no canonical events in the window
// are absent. A work's metrics never include its version rows.
func (st *Store) Metrics(ctx context.Context, tenant string, refs []ContentRef, window Window) (map[ContentKey]ContentMetrics, error) {
	out := map[ContentKey]ContentMetrics{}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return out, nil
	}
	for _, r := range refs {
		if err := checkRef(tenant, r); err != nil {
			return nil, err
		}
	}
	filter, filterArgs := refTuples(refs)
	q, args := st.windowMetrics(tenant, filter, filterArgs, window)
	rows, err := st.conn.Query(ctx, q+finalSettings, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind, id, version string
			m                 ContentMetrics
		)
		if err := scanMetrics(rows, &kind, &id, &version, &m); err != nil {
			return nil, fmt.Errorf("signal: metrics scan: %w", err)
		}
		out[contentref.NewVersion(tenant, kind, id, version).Key()] = m
	}
	return out, rows.Err()
}

// Popular ranks works of one kind with at least one view in a literal window.
// Every view in the window has equal time weight.
func (st *Store) Popular(ctx context.Context, tenant string, contentKind string, opts PopularOptions) ([]PopularHit, error) {
	return st.popular(ctx, tenant, contentKind, nil, opts)
}

// PopularityFor scores a fixed candidate set of works by the popularity
// ranking and returns content_id -> score. Candidates without views in the
// window are absent.
func (st *Store) PopularityFor(ctx context.Context, tenant string, contentKind string, ids []string, window Window) (map[string]float64, error) {
	ids = trimAll(ids)
	if len(ids) == 0 {
		return map[string]float64{}, nil
	}
	hits, err := st.popular(ctx, tenant, contentKind, ids, PopularOptions{Window: window, Limit: len(ids)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(hits))
	for _, h := range hits {
		out[h.ContentID] = h.Score
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

func (st *Store) popular(ctx context.Context, tenant string, contentKind string, ids []string, opts PopularOptions) ([]PopularHit, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("signal: tenant is required")
	}
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("signal: contentKind is required")
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
	filter, filterArgs := "content_kind = ?"+workLevel, []any{contentKind}
	if len(ids) > 0 {
		filter += " AND content_id IN ?"
		filterArgs = append(filterArgs, ids)
	}
	metrics, args := st.windowMetrics(tenant, filter, filterArgs, opts.Window)
	q := fmt.Sprintf(`SELECT %s, %s, (%s) AS rank_score
FROM (%s)
WHERE views > 0
ORDER BY rank_score DESC, content_id ASC
LIMIT ?%s`, refColumns, metricColumns, rankExpr, metrics, finalSettings)
	rows, err := st.conn.Query(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("signal: popular: %w", err)
	}
	defer rows.Close()
	out := make([]PopularHit, 0, limit)
	for rows.Next() {
		var (
			h                 PopularHit
			kind, id, version string
		)
		if err := scanMetrics(rows, &kind, &id, &version, &h.ContentMetrics, &h.Score); err != nil {
			return nil, fmt.Errorf("signal: popular scan: %w", err)
		}
		h.ContentRef = contentref.NewVersion(tenant, kind, id, version)
		if math.IsNaN(h.Score) || math.IsInf(h.Score, 0) {
			h.Score = 0
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CoEngaged returns works co-engaged with the anchor work: "subjects who
// engaged with X also engaged with Y". Strength nets subjects whose summed
// feedback for the candidate is non-negative against those with negative.
func (st *Store) CoEngaged(ctx context.Context, tenant string, ref ContentRef, opts CoEngagedOptions) ([]CoEngagedHit, error) {
	if err := checkRef(tenant, ref); err != nil {
		return nil, err
	}
	if ref.Version() != "" {
		return nil, fmt.Errorf("signal: co-engagement is work-level; %s names a version", ref)
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
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		candidates.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	args = append(args, predArgs...)
	args = append(args, ref.ContentKind, ref.ContentID, tenant, ref.ContentKind, ref.ContentID)
	args = append(args, predArgs...)
	args = append(args, maxSubjects, limit)
	q := fmt.Sprintf(`SELECT content_kind, content_id,
    toInt64(countIf(net >= 0)) - toInt64(countIf(net < 0)) AS strength
FROM (
    SELECT content_kind, content_id, subject_kind, subject, sum(value_sum) AS net
    FROM %[1]s.subject_content_daily FINAL
    WHERE tenant = ? AND events > 0%[6]s%[2]s%[3]s AND %[5]s
      AND NOT (content_kind = ? AND content_id = ?)
      AND (subject_kind, subject) IN (
          SELECT DISTINCT subject_kind, subject FROM %[1]s.subject_content_daily FINAL
          WHERE tenant = ? AND content_kind = ? AND content_id = ?%[6]s AND events > 0%[3]s AND %[5]s
          LIMIT ?)
    GROUP BY content_kind, content_id, subject_kind, subject
)
GROUP BY content_kind, content_id
HAVING strength > 0
ORDER BY strength DESC, content_kind ASC, content_id ASC
LIMIT ?%[4]s`, st.db, candidates.String(), pred, finalSettings, st.notErased(), workLevel)
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged: %w", err)
	}
	defer rows.Close()
	return scanCoEngaged(rows, tenant, limit)
}

func scanCoEngaged(rows driver.Rows, tenant string, limit int) ([]CoEngagedHit, error) {
	out := make([]CoEngagedHit, 0, limit)
	for rows.Next() {
		var (
			h        CoEngagedHit
			kind, id string
		)
		if err := rows.Scan(&kind, &id, &h.Strength); err != nil {
			return nil, fmt.Errorf("signal: co-engaged scan: %w", err)
		}
		h.ContentRef = contentref.New(tenant, kind, id)
		out = append(out, h)
	}
	return out, rows.Err()
}

// coEngagedFromRollup serves CoEngaged from the precomputed content_pairs table.
func (st *Store) coEngagedFromRollup(ctx context.Context, tenant string, ref ContentRef, opts CoEngagedOptions, limit int) ([]CoEngagedHit, error) {
	var sb strings.Builder
	args := []any{tenant, ref.ContentKind, ref.ContentID}
	fmt.Fprintf(&sb, `SELECT content_kind_b, content_id_b, max(strength) AS s
FROM %s.content_pairs
WHERE tenant = ? AND content_kind_a = ? AND content_id_a = ?`, st.db)
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind_b IN ?")
		args = append(args, kinds)
	}
	sb.WriteString("\nGROUP BY content_kind_b, content_id_b\nHAVING s > 0\nORDER BY s DESC, content_kind_b ASC, content_id_b ASC\nLIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged rollup: %w", err)
	}
	defer rows.Close()
	return scanCoEngaged(rows, tenant, limit)
}

// maxRefreshAttempts bounds RefreshCoEngagement's rebuilds when erasures keep
// landing while it computes.
const maxRefreshAttempts = 3

// RefreshCoEngagement (re)materializes the content_pairs rollup for one
// tenant: per subject, the distinct works with non-negative summed feedback
// (capped at MaxContentPerSubject) are cross-joined into pairs; strength =
// co-engaged subjects minus subjects with negative feedback on the candidate.
// Pairs carry no subject, so a build that started before an erasure could
// publish that subject's contribution after EraseSubjects removed its pairs:
// the build is repeated while the erasure ledger changed during it.
func (st *Store) RefreshCoEngagement(ctx context.Context, tenant string, opts RefreshCoEngagementOptions) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := opts.Window.Validate(); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		before, err := st.erasedCount(ctx, tenant)
		if err != nil {
			return err
		}
		if err := st.buildCoEngagement(ctx, tenant, opts); err != nil {
			return err
		}
		after, err := st.erasedCount(ctx, tenant)
		if err != nil {
			return err
		}
		if after == before {
			return nil
		}
		if attempt == maxRefreshAttempts {
			return fmt.Errorf("signal: co-engagement refresh raced erasures %d times; retry", attempt)
		}
	}
}

func (st *Store) buildCoEngagement(ctx context.Context, tenant string, opts RefreshCoEngagementOptions) error {
	maxPer := opts.MaxContentPerSubject
	if maxPer <= 0 {
		maxPer = 100
	}
	if err := st.conn.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.content_pairs WHERE tenant = ?`, st.db), tenant); err != nil {
		return fmt.Errorf("signal: clear content_pairs: %w", err)
	}
	winPred, winArgs := opts.Window.dayPredicate("day")
	q := fmt.Sprintf(`INSERT INTO %[1]s.content_pairs
(tenant, content_kind_a, content_id_a, content_kind_b, content_id_b, strength, refreshed_at)
SELECT
    '%[2]s' AS tenant,
    a.1 AS content_kind_a, a.2 AS content_id_a,
    (bs.1).1 AS content_kind_b, (bs.1).2 AS content_id_b,
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
                groupUniqArrayIf(%[3]d)((content_kind, content_id), net_v >= 0) AS pos,
                groupUniqArrayIf(%[3]d)((content_kind, content_id), net_v < 0) AS neg
            FROM (
                SELECT subject_kind, subject, content_kind, content_id, sum(value_sum) AS net_v
                FROM %[1]s.subject_content_daily FINAL
                WHERE tenant = ? AND events > 0%[7]s%[4]s AND %[6]s
                GROUP BY subject_kind, subject, content_kind, content_id
            )
            GROUP BY subject_kind, subject
        )
    )
)
WHERE a != bs.1
GROUP BY content_kind_a, content_id_a, content_kind_b, content_id_b
HAVING strength > 0%[5]s`, st.db, escapeCHString(tenant), maxPer, winPred, finalSettings, st.notErased(), workLevel)
	if err := st.conn.Exec(ctx, q, append([]any{tenant}, winArgs...)...); err != nil {
		return fmt.Errorf("signal: refresh co-engagement: %w", err)
	}
	return nil
}
