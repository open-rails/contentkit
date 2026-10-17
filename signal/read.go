package signal

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/open-rails/contentkit/contentref"
)

const stateColumns = refColumns + `, first_seen_at,last_signal_at,last_read_at,active_ms,
 max_progress,progress_max,completed,resume,quality,net_value`

// workLevel selects the rows of the work itself, never of a version.
const workLevel = " AND content_version_id = ''"

func scanStateRow(rows driver.Rows, tenant string) (StateRow, error) {
	var (
		r       StateRow
		version string
	)
	if err := rows.Scan(
		&r.ContentKind, &r.ContentID, &version, &r.FirstSeenAt, &r.LastSignalAt, &r.LastReadAt,
		&r.ActiveMS, &r.MaxProgress, &r.ProgressMax, &r.Completed, &r.Resume, &r.Quality, &r.NetValue,
	); err != nil {
		return StateRow{}, err
	}
	r.TenantID = tenant
	r.ContentRef = r.ContentRef.WithVersion(version)
	r.Seen = r.MaxProgress > 0
	return r, nil
}

const referenceFilter = "(" + refColumns + ") IN (SELECT " + refColumns + " FROM contentkit_read_refs)"

func referenceContext(ctx context.Context, refs []ContentRef) (context.Context, error) {
	table, err := ext.NewTable("contentkit_read_refs",
		ext.Column("content_kind", "String"),
		ext.Column("content_id", "String"),
		ext.Column("content_version_id", "String"),
	)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if err := table.Append(r.ContentKind, r.ContentID, r.Version()); err != nil {
			return nil, err
		}
	}
	return clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), nil
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
	ctx, err := referenceContext(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("signal: states references: %w", err)
	}
	args := []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}
	q := fmt.Sprintf(`SELECT %s
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND %s`,
		stateColumns, st.readingState(), st.subjectNotErased(), referenceFilter)
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

func historyFilter(sb *strings.Builder, args []any, opts HistoryOptions) ([]any, string, error) {
	if k := strings.TrimSpace(opts.ContentKind); k != "" {
		sb.WriteString(" AND content_kind = ?")
		args = append(args, k)
	}
	timeColumn := "last_read_at"
	sb.WriteString(" AND (active_ms>0 OR max_progress>0 OR completed OR resume!='')")
	switch opts.Status {
	case HistoryAny:
	case HistorySeen:
		sb.WriteString(" AND max_progress > 0")
	case HistoryInProgress:
		sb.WriteString(" AND max_progress > 0 AND NOT completed")
	case HistoryCompleted:
		sb.WriteString(" AND completed")
	default:
		return nil, "", fmt.Errorf("signal: invalid HistoryOptions.Status %q", opts.Status)
	}
	if !opts.Since.IsZero() {
		sb.WriteString(" AND " + timeColumn + " >= ?")
		args = append(args, opts.Since.UTC())
	}
	return args, timeColumn, nil
}

// History returns the subject's work-level state rows, most recent relevant
// activity first. Seen statuses use view recency; HistoryAny uses signal recency.
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
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.readingState(), st.subjectNotErased(), workLevel)
	args, timeColumn, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}, opts)
	if err != nil {
		return nil, err
	}
	sb.WriteString(" ORDER BY " + timeColumn + " DESC, content_kind ASC, content_id ASC LIMIT ? OFFSET ?")
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
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, st.readingState(), st.subjectNotErased(), workLevel)
	args, _, err := historyFilter(&sb, []any{tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}, opts)
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
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND content_kind = ?%s AND max_progress > 0`, st.readingState(), st.subjectNotErased(), workLevel)
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
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s AND net_value < 0`, st.feedbackState(), st.subjectNotErased(), workLevel)
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
// seeds): ordered by quality DESC, then recency.
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
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.readingState(), st.subjectNotErased(), workLevel)
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	if opts.ExcludeNegative {
		sb.WriteString(" AND (content_kind,content_id) NOT IN (SELECT content_kind,content_id FROM (" + st.feedbackState() + ") WHERE tenant=? AND subject_kind=? AND subject=? AND net_value<0)")
		args = append(args, tenant, subject.Kind(), subject.Key())
	}
	sb.WriteString(" AND quality > 0 ORDER BY quality DESC, last_signal_at DESC, content_kind ASC, content_id ASC LIMIT ?")
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
const metricColumns = `readers,user_readers,guest_readers,completers,active_ms,reader_quality_sum,
 returning_readers,value_sum,positive_subjects,negative_subjects`

// windowMetrics aggregates canonical measurements and feedback in a window:
// first per subject (so each person counts once), then per content reference.
func (st *Store) windowMetrics(tenant, filter string, filterArgs []any, win Window) (string, []any) {
	// Work filters also admit their version source rows. Filtering again after
	// aggregation preserves exact work/version requests without scanning other IDs.
	sourceFilter := strings.ReplaceAll(filter, workLevel, "")
	sourceFilter = strings.ReplaceAll(sourceFilter, referenceFilter, "("+referenceFilter+" OR (content_kind,content_id,'') IN (SELECT "+refColumns+" FROM contentkit_read_refs))")
	pred, predArgs := win.predicate("c.1")
	canonical := "SELECT * FROM (" + st.measuredCanonical(sourceFilter) + ") WHERE 1" + pred
	measured := measuredVersions(canonical)
	feedback := "SELECT * FROM (" + st.canonicalEvents(sourceFilter+" AND signal_type NOT IN ('view','preview')") + ") WHERE 1" + pred
	q := fmt.Sprintf(`WITH measured_rows AS (%s
 SELECT %s,subject_kind,subject,qualified,completed,active_ms,attention_quality AS quality,days FROM scored
 UNION ALL
 SELECT content_kind,content_id,'',subject_kind,subject,max(qualified),max(completed),sum(active_ms),max(attention_quality),groupUniqArrayArray(days)
 FROM scored GROUP BY content_kind,content_id,subject_kind,subject),
 feedback_rows AS (SELECT %s,subject_kind,subject,sum(c.3) AS value FROM (%s)
 GROUP BY %s,subject_kind,subject),
 combined AS (SELECT %s,subject_kind,subject,max(qualified) AS qualified,max(completed) AS completed,
 sum(active_ms) AS active_ms,max(quality) AS quality,groupUniqArrayArray(days) AS days,sum(value) AS value FROM (
 SELECT %s,subject_kind,subject,qualified,completed,active_ms,quality,days,toFloat64(0) AS value FROM measured_rows
 UNION ALL
 SELECT %s,subject_kind,subject,false,false,toUInt64(0),toFloat64(0),CAST([],'Array(Date)'),value FROM feedback_rows)
 GROUP BY %s,subject_kind,subject)
 SELECT %s,toUInt64(countIf(qualified)) AS readers,
 toUInt64(countIf(qualified AND subject_kind='user')) AS user_readers,
 toUInt64(countIf(qualified AND subject_kind='anon')) AS guest_readers,
 toUInt64(countIf(completed)) AS completers,
 sum(active_ms) AS active_ms,
 sumIf(quality,qualified) AS reader_quality_sum,toUInt64(countIf(qualified AND length(days)>1)) AS returning_readers,
 sum(value) AS value_sum,toUInt64(countIf(value>0)) AS positive_subjects,
 toUInt64(countIf(value<0)) AS negative_subjects
 FROM combined WHERE %s GROUP BY %s`, measured, refColumns, refColumns, feedback, refColumns,
		refColumns, refColumns, refColumns, refColumns, refColumns, filter, refColumns)
	args := append([]any{tenant}, filterArgs...)
	args = append(args, predArgs...)
	args = append(args, tenant)
	args = append(args, filterArgs...)
	args = append(args, predArgs...)
	args = append(args, filterArgs...)
	return q, args
}

func scanMetrics(rows driver.Rows, kind, id, version *string, m *ContentMetrics, extra ...any) error {
	dest := []any{kind, id, version, &m.Readers, &m.UserReaders, &m.GuestReaders, &m.Completers, &m.ActiveMS,
		&m.ReaderQualitySum, &m.ReturningReaders, &m.ValueSum, &m.PositiveSubjects, &m.NegativeSubjects}
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
	ctx, err := referenceContext(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("signal: metrics references: %w", err)
	}
	q, args := st.windowMetrics(tenant, referenceFilter, nil, window)
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

// Popular ranks works of one kind with qualified readers in a literal window.
// Measured observations have equal time weight inside that window.
func (st *Store) Popular(ctx context.Context, tenant string, contentKind string, opts PopularOptions) ([]PopularHit, error) {
	return st.popular(ctx, tenant, contentKind, nil, opts)
}

// PopularityFor scores a fixed candidate set of works by the popularity
// ranking and returns content_id -> score. Candidates without qualified
// readers in the window are absent.
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
		"log10(1 + toFloat64(readers)) * greatest(%g, (toFloat64(reader_quality_sum)*100 + %g) / (toFloat64(readers) + %g))",
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
	if opts.Normalization != nil {
		return st.normalizedPopular(ctx, tenant, contentKind, ids, opts)
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
		table, err := ext.NewTable("contentkit_popularity_ids", ext.Column("content_id", "String"))
		if err != nil {
			return nil, fmt.Errorf("signal: popularity candidates: %w", err)
		}
		for _, id := range ids {
			if err := table.Append(id); err != nil {
				return nil, fmt.Errorf("signal: popularity candidates: %w", err)
			}
		}
		ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(table))
		filter += " AND content_id IN (SELECT content_id FROM contentkit_popularity_ids)"
	}
	metrics, args := st.windowMetrics(tenant, filter, filterArgs, opts.Window)
	q := fmt.Sprintf(`SELECT %s, %s, (%s) AS rank_score
FROM (%s)
WHERE readers > 0
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
	measured, args := st.measuredSubjectWorks(tenant, opts.Window)
	var candidates strings.Builder
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		candidates.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	args = append(args, ref.ContentKind, ref.ContentID, ref.ContentKind, ref.ContentID, maxSubjects, limit)
	q := fmt.Sprintf(`WITH readers AS (%s)
 SELECT content_kind,content_id,toInt64(countIf(net_value>=0))-toInt64(countIf(net_value<0)) AS strength
 FROM readers WHERE 1%s AND NOT(content_kind=? AND content_id=?)
 AND (subject_kind,subject) IN (SELECT subject_kind,subject FROM readers WHERE content_kind=? AND content_id=? LIMIT ?)
 GROUP BY content_kind,content_id HAVING strength>0 ORDER BY strength DESC,content_kind,content_id LIMIT ?%s`, measured, candidates.String(), finalSettings)

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
FROM %s.engagement_pairs
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
	source, args := st.measuredSubjectWorks(tenant, opts.Window)
	if err := st.conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s.engagement_pairs WHERE tenant=?", st.db), tenant); err != nil {
		return err
	}
	// A fresh token per build: replicated insert deduplication would otherwise
	// drop a rebuild identical to the block just deleted, leaving no pairs.
	q := fmt.Sprintf(`INSERT INTO %s.engagement_pairs
 (tenant,content_kind_a,content_id_a,content_kind_b,content_id_b,strength,refreshed_at)
 SELECT ?,a.1,a.2,b.1.1,b.1.2,toInt64(sum(b.2)),now() FROM (
 SELECT arrayJoin(positive) AS a,arrayJoin(arrayConcat(arrayMap(x->tuple(x,1),positive),arrayMap(x->tuple(x,-1),negative))) AS b FROM (
 SELECT subject_kind,subject,groupUniqArrayIf(%d)(tuple(content_kind,content_id),net_value>=0) AS positive,
 groupUniqArrayIf(%d)(tuple(content_kind,content_id),net_value<0) AS negative
 FROM (%s) GROUP BY subject_kind,subject)) WHERE a!=b.1 GROUP BY a,b.1 HAVING sum(b.2)>0
 SETTINGS insert_deduplication_token = ?`, st.db, maxPer, maxPer, source)
	args = append(append([]any{tenant}, args...), contentref.NewID())
	return st.conn.Exec(ctx, q, args...)
}

// DayActivity reports distinct people with measured attention, not page opens,
// by audience: guests, signed-in people without a tier, and premium people.
type DayActivity struct {
	Day      time.Time
	Readers  uint64
	Anon     uint64
	Free     uint64
	Premium  uint64
	ActiveMS uint64
}

func (st *Store) Activity(ctx context.Context, tenant, contentKind string, window Window) ([]DayActivity, error) {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("signal: tenant and contentKind are required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	pred, args := window.dayPredicate("day")
	observed, observedArgs := window.dayPredicate("toDate(observed_at)")
	// Premium needs a known tier observed that day; other signed-in people are free.
	q := fmt.Sprintf(`SELECT d.day,toUInt64(count()),toUInt64(countIf(d.subject_kind<>'user')),
 toUInt64(countIf(d.subject_kind='user' AND ifNull(p.premium,0)=0)),
 toUInt64(countIf(d.subject_kind='user' AND ifNull(p.premium,0)=1)),sum(d.active) FROM (
 SELECT day,subject_kind,subject,sum(active_ms) AS active,sum(length(covered_units)) AS units
 FROM %[1]s.engagement_daily FINAL WHERE tenant=? AND content_kind=? AND present%[2]s AND %[3]s
 GROUP BY day,subject_kind,subject HAVING active>=1000 AND units>0) d
 LEFT JOIN (SELECT toDate(observed_at) AS day,subject_kind,subject,toUInt8(max(eligibility_known AND notEmpty(eligible_tiers))) AS premium
 FROM %[1]s.measured_population_observations FINAL WHERE tenant=? AND content_kind=?%[4]s
 GROUP BY day,subject_kind,subject) p ON p.day=d.day AND p.subject_kind=d.subject_kind AND p.subject=d.subject
 GROUP BY d.day ORDER BY d.day DESC%[5]s`, st.db, pred, st.notErased(), observed, finalSettings)
	params := append([]any{tenant, contentKind}, args...)
	params = append(append(params, tenant, contentKind), observedArgs...)
	rows, err := st.conn.Query(ctx, q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayActivity
	for rows.Next() {
		var row DayActivity
		if err := rows.Scan(&row.Day, &row.Readers, &row.Anon, &row.Free, &row.Premium, &row.ActiveMS); err != nil {
			return nil, err
		}
		row.Day = utcDay(row.Day)
		out = append(out, row)
	}
	return out, rows.Err()
}
