package signalstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/open-rails/contentkit/contentref"
	signal "github.com/open-rails/contentkit/signal"
)

const stateColumns = refColumns + `, first_seen_at,last_signal_at,last_read_at,active_ms,
 max_progress,progress_max,completed,resume,quality,net_value`

// workLevel selects the rows of the work itself, never of a version.
const workLevel = " AND content_version_id = ''"

func scanStateRow(rows driver.Rows, tenant string) (signal.StateRow, error) {
	var (
		r       signal.StateRow
		version string
	)
	if err := rows.Scan(
		&r.ContentKind, &r.ContentID, &version, &r.FirstSeenAt, &r.LastSignalAt, &r.LastReadAt,
		&r.ActiveMS, &r.MaxProgress, &r.ProgressMax, &r.Completed, &r.Resume, &r.Quality, &r.NetValue,
	); err != nil {
		return signal.StateRow{}, err
	}
	r.TenantID = tenant
	r.ContentRef = r.ContentRef.WithVersion(version)
	r.Seen = r.MaxProgress > 0
	return r, nil
}

const referenceFilter = "(" + refColumns + ") IN (SELECT " + refColumns + " FROM contentkit_read_refs)"

func referenceContext(ctx context.Context, refs []signal.ContentRef) (context.Context, error) {
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
func (st *Store) States(ctx context.Context, tenant string, subject signal.Subject, refs []signal.ContentRef) (map[signal.ContentKey]signal.State, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	out := make(map[signal.ContentKey]signal.State, len(refs))
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
	args := append(scopeArgs(tenant, subject.Kind(), subject.Key()), tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject))
	q := fmt.Sprintf(`SELECT %s
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND %s`,
		stateColumns, st.readingState(subjectScope+" AND "+referenceFilter), st.subjectNotErased(), referenceFilter)
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

func historyFilter(sb *strings.Builder, args []any, opts signal.HistoryOptions) ([]any, string, error) {
	if k := strings.TrimSpace(opts.ContentKind); k != "" {
		sb.WriteString(" AND content_kind = ?")
		args = append(args, k)
	}
	timeColumn := "last_read_at"
	sb.WriteString(" AND (active_ms>0 OR max_progress>0 OR completed OR resume!='')")
	switch opts.Status {
	case signal.HistoryAny:
	case signal.HistorySeen:
		sb.WriteString(" AND max_progress > 0")
	case signal.HistoryInProgress:
		sb.WriteString(" AND max_progress > 0 AND NOT completed")
	case signal.HistoryCompleted:
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
func (st *Store) History(ctx context.Context, tenant string, subject signal.Subject, opts signal.HistoryOptions) ([]signal.StateRow, error) {
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
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.readingState(subjectScope), st.subjectNotErased(), workLevel)
	args, timeColumn, err := historyFilter(&sb, append(scopeArgs(tenant, subject.Kind(), subject.Key()), tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)), opts)
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
	out := make([]signal.StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows, tenant)
		if err != nil {
			return nil, fmt.Errorf("signal: history scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeenIDs returns the subject's seen-set for one content kind: work ids with
// max_progress > 0. This is the signal-plane half of the unseen anti-join.
func (st *Store) SeenIDs(ctx context.Context, tenant string, subject signal.Subject, contentKind string) (map[string]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("signal: contentKind is required")
	}
	q := fmt.Sprintf(`SELECT content_id
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s AND content_kind = ?%s AND max_progress > 0`, st.readingState(subjectScope), st.subjectNotErased(), workLevel)
	rows, err := st.conn.Query(ctx, q, append(scopeArgs(tenant, subject.Kind(), subject.Key()), tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject), contentKind)...)
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
func (st *Store) NegativeIDs(ctx context.Context, tenant string, subject signal.Subject, contentKinds []string) (map[signal.ContentKey]struct{}, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	var sb strings.Builder
	args := []any{tenant, subject.Kind(), subject.Key(), tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject)}
	fmt.Fprintf(&sb, `SELECT content_kind, content_id
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s AND net_value < 0`, st.feedbackState(subjectScope), st.subjectNotErased(), workLevel)
	if kinds := trimAll(contentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: negative ids: %w", err)
	}
	defer rows.Close()
	out := map[signal.ContentKey]struct{}{}
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
func (st *Store) TopStates(ctx context.Context, tenant string, subject signal.Subject, opts signal.TopStatesOptions) ([]signal.StateRow, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	var sb strings.Builder
	args := append(scopeArgs(tenant, subject.Kind(), subject.Key()), tenant, subject.Kind(), subject.Key(), tenant, subjectHashKey(subject))
	fmt.Fprintf(&sb, `SELECT %s
FROM (%s)
WHERE tenant = ? AND subject_kind = ? AND subject = ? AND %s%s`, stateColumns, st.readingState(subjectScope), st.subjectNotErased(), workLevel)
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		sb.WriteString(" AND content_kind IN ?")
		args = append(args, kinds)
	}
	if opts.ExcludeNegative {
		sb.WriteString(" AND (content_kind,content_id) NOT IN (SELECT content_kind,content_id FROM (" + st.feedbackState(subjectScope) + ") WHERE tenant=? AND subject_kind=? AND subject=? AND net_value<0)")
		args = append(args, tenant, subject.Kind(), subject.Key(), tenant, subject.Kind(), subject.Key())
	}
	sb.WriteString(" AND quality > 0 ORDER BY quality DESC, last_signal_at DESC, content_kind ASC, content_id ASC LIMIT ?")
	args = append(args, limit)
	rows, err := st.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("signal: top states: %w", err)
	}
	defer rows.Close()
	out := make([]signal.StateRow, 0, limit)
	for rows.Next() {
		r, err := scanStateRow(rows, tenant)
		if err != nil {
			return nil, fmt.Errorf("signal: top states scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// finalSettings: day is in the sorting key and partition key, so FINAL never
// needs to merge across partitions.
const finalSettings = "\nSETTINGS do_not_merge_across_partitions_select_final = 1"

// CoEngaged returns works co-engaged with the anchor work: "subjects who
// engaged with X also engaged with Y". Strength nets subjects whose summed
// feedback for the candidate is non-negative against those with negative.
func (st *Store) CoEngaged(ctx context.Context, tenant string, ref signal.ContentRef, opts signal.CoEngagedOptions) ([]signal.CoEngagedHit, error) {
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
	readers, err := st.coEngagedReaders(ctx, tenant, ref, opts.Window, maxSubjects)
	if err != nil {
		return nil, err
	}
	if readers.Block().Rows() == 0 {
		return []signal.CoEngagedHit{}, nil
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(readers))
	window, windowArgs := windowPredicate(opts.Window, "c.1")
	scope := "(subject_kind,subject) IN (SELECT subject_kind,subject FROM contentkit_coengaged_readers) AND NOT(content_kind=? AND content_id=?)"
	scopeArgs := []any{ref.ContentKind, ref.ContentID}
	if kinds := trimAll(opts.ContentKinds); len(kinds) > 0 {
		scope += " AND content_kind IN ?"
		scopeArgs = append(scopeArgs, kinds)
	}
	candidates := "SELECT * FROM (" + st.measuredCanonical(scope) + ") WHERE 1" + window
	args := []any{tenant}
	args = append(args, scopeArgs...)
	args = append(args, windowArgs...)
	args = append(args, tenant)
	args = append(args, scopeArgs...)
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT content_kind,content_id,toInt64(countIf(net_value>=0))-toInt64(countIf(net_value<0)) AS strength
 FROM (SELECT m.content_kind,m.content_id,coalesce(f.net_value,0.) AS net_value FROM (%s
 SELECT content_kind,content_id,subject_kind,subject FROM scored
 GROUP BY content_kind,content_id,subject_kind,subject HAVING max(qualified)) m
 LEFT JOIN (%s) f USING(content_kind,content_id,subject_kind,subject))
 GROUP BY content_kind,content_id HAVING strength>0 ORDER BY strength DESC,content_kind,content_id LIMIT ?%s`,
		measuredVersions(candidates), st.feedbackState("tenant=? AND "+scope+" AND content_version_id=''"), finalSettings)

	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged: %w", err)
	}
	defer rows.Close()
	return scanCoEngaged(rows, tenant, limit)
}

// Send the bounded sample as query-local data, not SQL literals or a CTE that
// ClickHouse would evaluate again for each candidate source.
func (st *Store) coEngagedReaders(ctx context.Context, tenant string, ref signal.ContentRef, window signal.Window, limit int) (*ext.Table, error) {
	pred, windowArgs := windowPredicate(window, "c.1")
	canonical := "SELECT * FROM (" + st.measuredCanonical("content_kind=? AND content_id=?") + ") WHERE 1" + pred
	args := append([]any{tenant, ref.ContentKind, ref.ContentID}, windowArgs...)
	args = append(args, limit)
	q := measuredVersions(canonical) + `
 SELECT subject_kind,subject FROM scored GROUP BY subject_kind,subject HAVING max(qualified)
 ORDER BY cityHash64(subject_kind,subject),subject_kind,subject LIMIT ?` + finalSettings
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: co-engaged readers: %w", err)
	}
	defer rows.Close()
	table, err := ext.NewTable("contentkit_coengaged_readers", ext.Column("subject_kind", "String"), ext.Column("subject", "String"))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, subject string
		if err := rows.Scan(&kind, &subject); err != nil {
			return nil, fmt.Errorf("signal: co-engaged reader scan: %w", err)
		}
		if err := table.Append(kind, subject); err != nil {
			return nil, err
		}
	}
	return table, rows.Err()
}

func scanCoEngaged(rows driver.Rows, tenant string, limit int) ([]signal.CoEngagedHit, error) {
	out := make([]signal.CoEngagedHit, 0, limit)
	for rows.Next() {
		var (
			h        signal.CoEngagedHit
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

func (st *Store) Activity(ctx context.Context, tenant, contentKind string, window signal.Window, cohorts ...signal.ActivityCohort) ([]signal.DayActivity, error) {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("signal: tenant and contentKind are required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if err := signal.ValidateActivityCohorts(cohorts); err != nil {
		return nil, err
	}
	pred, args := windowDayPredicate(window, "day")
	cohortSQL, params := activityCohortSQL(cohorts)
	join := ""
	var populationArgs []any
	for _, c := range cohorts {
		if len(c.AnyTier) == 0 {
			continue
		}
		observed, observedArgs := windowDayPredicate(window, "toDate(observed_at)")
		join = fmt.Sprintf(`LEFT JOIN (
 SELECT toDate(observed_at) AS day,subject_kind,subject,
 arrayDistinct(arrayFlatten(groupArrayIf(eligible_tiers,eligibility_known))) AS tiers
 FROM %s.measured_population_observations FINAL WHERE tenant=? AND content_kind=?%s
 GROUP BY day,subject_kind,subject) p ON p.day=d.day AND p.subject_kind=d.subject_kind AND p.subject=d.subject`, st.db, observed)
		populationArgs = append([]any{tenant, contentKind}, observedArgs...)
		break
	}
	q := fmt.Sprintf(`SELECT d.day,%[4]s AS cohort,toUInt64(count()),sum(d.active) FROM (
 SELECT day,subject_kind,subject,sum(active_ms) AS active,sum(length(covered_units)) AS units
 FROM %[1]s.engagement_daily FINAL WHERE tenant=? AND content_kind=? AND present%[2]s AND %[3]s
 GROUP BY day,subject_kind,subject HAVING active>=1000 AND units>0) d
 %[5]s GROUP BY d.day,cohort ORDER BY d.day DESC,cohort%[6]s`, st.db, pred, st.notErased(), cohortSQL, join, finalSettings)
	params = append(params, tenant, contentKind)
	params = append(params, args...)
	params = append(params, populationArgs...)
	rows, err := st.conn.Query(ctx, q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []signal.DayActivity
	for rows.Next() {
		var day time.Time
		var cohort string
		var readers, active uint64
		if err := rows.Scan(&day, &cohort, &readers, &active); err != nil {
			return nil, err
		}
		day = utcDay(day)
		if len(out) == 0 || !out[len(out)-1].Day.Equal(day) {
			out = append(out, signal.DayActivity{Day: day, Cohorts: map[string]uint64{}})
		}
		row := &out[len(out)-1]
		row.Readers += readers
		row.ActiveMS += active
		if cohort != "" {
			row.Cohorts[cohort] = readers
		}
	}
	return out, rows.Err()
}
