package signalstore

import (
	"context"
	"fmt"

	signal "github.com/open-rails/contentkit/signal"
)

// readingState relates measured coverage to the retained legacy resume copy.
// Legacy rows can restore progress, but never become quality/ranking evidence.
// scope, a predicate over the source rows such as subjectScope, is applied
// inside each source read: a filter outside the full join reaches no table.
// Its placeholders repeat once per source, in scopeArgs order.
func (st *Store) readingState(scope string) string {
	reading := fmt.Sprintf(`SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
 min(first_at) AS first_seen_at,max(last_at) AS last_signal_at,max(last_at) AS last_read_at,
 sum(active_ms) AS active_ms,
 tupleElement(argMax(tuple(progress,total),tuple(ratio,last_at)),1) AS max_progress,
 tupleElement(argMax(tuple(progress,total),tuple(ratio,last_at)),2) AS progress_max,
 max(done) AS completed,argMaxIf(resume,last_at,resume!='') AS resume,
 max(quality) AS quality,toFloat64(0) AS net_value
 FROM (
 SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
 first_engaged_at AS first_at,last_engaged_at AS last_at,active_ms,
 toUInt32(if((length(arrayFilter(unit -> unit_scale=1 OR unit_attention[intDiv(unit,unit_scale)]>=1000,covered_units)) AS read_units)=0,
  0,greatest(1,intDiv(read_units,greatest(unit_scale,1))))) AS progress,
 toUInt32(intDiv(unit_total,greatest(unit_scale,1))) AS total,coverage AS ratio,completed AS done,resume,if(qualified,quality,0.) AS quality
 FROM %s.engagement_state FINAL WHERE %s
 UNION ALL
 SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
 first_seen_at,last_seen_at,toUInt64(0),progress,progress_max,
 toFloat64(progress)/greatest(toFloat64(progress_max),1.),completed,resume,toFloat64(0)
 FROM %s.legacy_reading_state FINAL WHERE %s)
 GROUP BY tenant,subject_kind,subject,content_kind,content_id,content_version_id`, st.db, scope, st.db, scope)
	return "SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id," +
		"if(r.first_seen_at=toDateTime(0,'UTC'),f.first_signal_at,r.first_seen_at) AS first_seen_at," +
		"greatest(r.last_signal_at,f.last_signal_at) AS last_signal_at,r.last_read_at,r.active_ms,r.max_progress,r.progress_max,r.completed,r.resume,r.quality,coalesce(f.net_value,0.) AS net_value FROM (" + reading + ") r FULL OUTER JOIN (" + st.feedbackState(scope) + ") f USING(tenant,subject_kind,subject,content_kind,content_id,content_version_id)"

}

// Explicit reactions remain separate from measured consumption. Retired open
// rows can coexist during rollout, but can never influence this relation.
func (st *Store) feedbackState(scope string) string {
	return fmt.Sprintf(`SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,
 sum(value) AS net_value,min(at) AS first_signal_at,max(at) AS last_signal_at FROM (
 SELECT tenant,subject_kind,subject,content_kind,content_id,content_version_id,event_id,signal_type,
 argMax(value,version) AS value,argMax(occurred_at,version) AS at FROM %s.signals
 WHERE signal_type NOT IN ('view','preview') AND %s AND %s
 GROUP BY tenant,subject_kind,subject,content_kind,content_id,content_version_id,event_id,signal_type)
 GROUP BY tenant,subject_kind,subject,content_kind,content_id,content_version_id`, st.db, st.notErased(), scope)
}

// CompletedCount is measured completions only. Retained legacy resume data is
// useful history, but is never promoted into measured account statistics.
func (st *Store) CompletedCount(ctx context.Context, tenant string, subject signal.Subject, kind string) (int64, error) {
	if err := subject.Validate(); err != nil {
		return 0, err
	}
	q := fmt.Sprintf("SELECT toInt64(count()) FROM %s.engagement_state FINAL WHERE tenant=? AND subject_kind=? AND subject=? AND content_kind=? AND content_version_id='' AND qualified AND completed AND %s", st.db, st.subjectNotErased())
	rows, err := st.conn.Query(ctx, q, tenant, subject.Kind(), subject.Key(), kind, tenant, subjectHashKey(subject))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return 0, err
		}
	}
	return count, rows.Err()
}

// subjectScope keeps one subject's rows; its arguments are scopeArgs.
const subjectScope = "tenant = ? AND subject_kind = ? AND subject = ?"

// scopeArgs repeats a scope's arguments for each of readingState's three
// source reads.
func scopeArgs(args ...any) []any {
	out := make([]any, 0, 3*len(args))
	for range 3 {
		out = append(out, args...)
	}
	return out
}
