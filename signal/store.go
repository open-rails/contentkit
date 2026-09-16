package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Conn is the minimal ClickHouse surface the Store needs; clickhouse-go's
// driver.Conn satisfies it.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// Store reads and writes the signal plane for one ClickHouse database.
// Every method is tenant-scoped via its tenant argument; the embedded hub
// pins a single tenant value.
type Store struct {
	conn Conn
	db   string
}

// NewStore returns a Store over an existing ClickHouse connection. The schema
// comes from migrations.SignalClickHouse; verify it with CheckSchema.
func NewStore(conn Conn, database string) (*Store, error) {
	if conn == nil {
		return nil, fmt.Errorf("signal: conn is required")
	}
	db := strings.TrimSpace(database)
	if db == "" {
		return nil, fmt.Errorf("signal: database is required")
	}
	if !identRe.MatchString(db) {
		return nil, fmt.Errorf("signal: invalid database name %q", db)
	}
	return &Store{conn: conn, db: db}, nil
}

// ProjectionKey identifies one subject × entity projection.
type ProjectionKey struct {
	EntityRef
	Subject Subject
}

func (k ProjectionKey) args() []any {
	return []any{k.EntityType, k.EntityID, k.Subject.Kind(), k.Subject.Key()}
}

func (k ProjectionKey) less(o ProjectionKey) bool {
	a, b := k.args(), o.args()
	for i := range a {
		if a[i] != b[i] {
			return a[i].(string) < b[i].(string)
		}
	}
	return false
}

// keyFilter renders "(entity_type, entity_id, subject_kind, subject) IN (...)"
// for sorted, deduplicated keys.
func keyFilter(keys []ProjectionKey) (string, []any) {
	tuples := make([]string, len(keys))
	args := make([]any, 0, len(keys)*4)
	for i, k := range keys {
		tuples[i] = "(?, ?, ?, ?)"
		args = append(args, k.args()...)
	}
	return "(entity_type, entity_id, subject_kind, subject) IN (" + strings.Join(tuples, ", ") + ")", args
}

// RecordSignals appends source events, then rebuilds the compact state and
// daily projections of every touched subject × entity from its canonical
// events. Replays, reordering and revisions converge; nothing is incremented.
// Signals of erased subjects (EraseSubjects) are dropped. If the process stops
// between the insert and the projections, the events are durable and
// RepairProjections restores the projections.
func (st *Store) RecordSignals(ctx context.Context, tenant string, signals []Signal) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if len(signals) == 0 {
		return nil
	}
	if len(signals) > MaxSignalsPerBatch {
		return &LimitError{Field: "signals per batch", Limit: MaxSignalsPerBatch, Got: len(signals)}
	}
	subjects := make([]Subject, 0, len(signals))
	for i := range signals {
		if err := signals[i].validate(); err != nil {
			return err
		}
		subjects = append(subjects, signals[i].Subject)
	}
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil {
		return err
	}
	args := make([]any, 0, len(signals)*17)
	rows := make([]string, 0, len(signals))
	touched := map[ProjectionKey]struct{}{}
	for i := range signals {
		s := signals[i]
		if _, erased := fenced[[2]string{s.Subject.Kind(), s.Subject.Key()}]; erased {
			continue
		}
		payload := ""
		if len(s.Payload) > 0 {
			b, err := json.Marshal(s.Payload)
			if err != nil {
				return fmt.Errorf("signal: marshal payload: %w", err)
			}
			payload = string(b)
		}
		if err := checkLen("Payload", payload, MaxPayloadBytes); err != nil {
			return err
		}
		// Use a server-side SELECT filter instead of a client-side fence check
		// alone. A request can be paused after fenced() and resume after an
		// erasure commits; evaluating the ledger in this statement prevents that
		// in-flight write from landing on any replica.
		rows = append(rows, "SELECT ? AS tenant, ? AS entity_type, ? AS entity_id, ? AS subject_kind, ? AS subject, ? AS signal_type, ? AS event_id, ? AS revision, ? AS occurred_at, ? AS duration_s, ? AS progress, ? AS progress_max, ? AS value, ? AS score, ? AS completed, ? AS resume, ? AS payload")
		args = append(args,
			tenant, s.EntityType, s.EntityID, s.Subject.Kind(), s.Subject.Key(), s.Type,
			strings.TrimSpace(s.EventID), s.Revision, s.OccurredAt.UTC(),
			s.DurationS, s.Progress, s.ProgressMax, s.Value, s.Score, s.Completed, s.Resume, payload,
		)
		touched[ProjectionKey{EntityRef: s.EntityRef, Subject: subjectFromKey(s.Subject.Kind(), s.Subject.Key())}] = struct{}{}
	}
	if len(rows) == 0 {
		return nil
	}
	insert := fmt.Sprintf(`INSERT INTO %s.events
(tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, revision, occurred_at,
 duration_s, progress, progress_max, value, score, completed, resume, payload)
SELECT tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, revision, occurred_at,
       duration_s, progress, progress_max, value, score, completed, resume, payload
FROM (%s) AS incoming
WHERE %s`, st.db, strings.Join(rows, " UNION ALL "), st.notErased())
	if err := st.conn.Exec(ctx, insert, args...); err != nil {
		return fmt.Errorf("signal: insert events: %w", err)
	}
	keys := make([]ProjectionKey, 0, len(touched))
	for k := range touched {
		keys = append(keys, k)
	}
	return st.project(ctx, tenant, keys)
}

// canonicalEvents selects, per logical event of the filtered keys, the
// highest-version row as c = (occurred_at, revision, duration_s, progress,
// progress_max, value, score, completed, resume) plus the newest ingest time of
// any of its rows. Superseded and duplicate rows never count, whether or not
// ClickHouse has merged them.
func (st *Store) canonicalEvents(filter string) string {
	return fmt.Sprintf(`SELECT entity_type, entity_id, subject_kind, subject, signal_type, event_id,
        argMax(tuple(occurred_at, revision, duration_s, progress, progress_max, value, score, completed, resume), version) AS c,
        max(ingested_at) AS ing
    FROM %s.events
    WHERE tenant = ? AND %s AND %s
    GROUP BY entity_type, entity_id, subject_kind, subject, signal_type, event_id`, st.db, filter, st.notErased())
}

// project rebuilds subject_state and subject_daily for keys from canonical
// events. Every derived row carries version = newest raw ingest time of its
// key, so a projection that saw more events replaces one that saw fewer and a
// late stale projection cannot win. Day rows that lost their canonical events
// (a revision moved a session to another day) are rewritten as zeros.
func (st *Store) project(ctx context.Context, tenant string, keys []ProjectionKey) error {
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	filter, keyArgs := keyFilter(keys)
	canon := st.canonicalEvents(filter)

	state := fmt.Sprintf(`INSERT INTO %[1]s.subject_state
(tenant, subject_kind, subject, entity_type, entity_id, first_seen_at, last_signal_at, total_events, views,
 completions, active_s, max_progress, progress_max, completed, resume, last_score, net_value, feedback, version)
SELECT ?, subject_kind, subject, entity_type, entity_id,
    min(c.1), max(c.1), toUInt32(count()),
    toUInt32(countIf(signal_type = '%[3]s')),
    toUInt32(countIf(signal_type = '%[3]s' AND c.8)),
    sumIf(toUInt64(c.3), signal_type = '%[3]s'),
    maxIf(c.4, signal_type = '%[3]s'),
    maxIf(c.5, signal_type = '%[3]s'),
    countIf(signal_type = '%[3]s' AND c.8) > 0,
    argMaxIf(c.9, (c.1, c.2, event_id), c.9 != ''),
    argMaxIf(c.7, (c.1, c.2, event_id), signal_type = '%[3]s'),
    sum(c.6),
    toUInt32(countIf(c.6 != 0)),
    max(ing)
FROM (%[2]s)
GROUP BY subject_kind, subject, entity_type, entity_id`, st.db, canon, TypeView)
	if err := st.conn.Exec(ctx, state, append([]any{tenant, tenant}, keyArgs...)...); err != nil {
		return fmt.Errorf("signal: project state: %w", err)
	}

	daily := fmt.Sprintf(`INSERT INTO %[1]s.subject_daily
(tenant, entity_type, entity_id, subject_kind, subject, day, events, views, completions, active_s,
 score_sum, value_sum, type_counts, version)
SELECT ?, entity_type, entity_id, subject_kind, subject, day, events, views, completions, active_s,
    score_sum, value_sum, type_counts, version
FROM (
    SELECT entity_type, entity_id, subject_kind, subject, day,
        toUInt32(sum(n)) AS events, toUInt32(sum(v)) AS views, toUInt32(sum(done)) AS completions,
        sum(active) AS active_s, sum(score) AS score_sum, sum(val) AS value_sum, sumMap(types) AS type_counts,
        max(max(ing)) OVER (PARTITION BY entity_type, entity_id, subject_kind, subject) AS version,
        max(prior) AS prior_events
    FROM (
        SELECT entity_type, entity_id, subject_kind, subject, toDate(c.1) AS day,
            toUInt32(1) AS n, toUInt32(signal_type = '%[4]s') AS v, toUInt32(signal_type = '%[4]s' AND c.8) AS done,
            if(signal_type = '%[4]s', toUInt64(c.3), 0) AS active, if(signal_type = '%[4]s', toInt64(c.7), 0) AS score,
            c.6 AS val, map(signal_type, toUInt32(1)) AS types, ing, toUInt32(0) AS prior
        FROM (%[2]s)
        UNION ALL
        SELECT entity_type, entity_id, subject_kind, subject, day, 0, 0, 0, 0, 0, 0,
            CAST(map(), 'Map(LowCardinality(String), UInt32)'), toDateTime64(0, 6, 'UTC'), events
        FROM %[1]s.subject_daily FINAL
        WHERE tenant = ? AND %[3]s
    )
    GROUP BY entity_type, entity_id, subject_kind, subject, day
    HAVING events > 0 OR prior_events > 0
)
WHERE version > toDateTime64(0, 6, 'UTC')`, st.db, canon, filter, TypeView)
	dailyArgs := append([]any{tenant, tenant}, keyArgs...)
	dailyArgs = append(dailyArgs, tenant)
	dailyArgs = append(dailyArgs, keyArgs...)
	if err := st.conn.Exec(ctx, daily, dailyArgs...); err != nil {
		return fmt.Errorf("signal: project daily: %w", err)
	}
	return nil
}

// RepairOptions bounds one RepairProjections call.
type RepairOptions struct {
	// Window limits candidate keys to those with an event on these days
	// (zero = all time).
	Window Window
	// IngestedSince limits candidates to keys with a row ingested at or after
	// it: the crash-repair scan. Zero = no bound.
	IngestedSince time.Time
	// After resumes strictly after this key in key order (nil = start).
	After *ProjectionKey
	// Limit caps candidate keys examined per call (default 1000).
	Limit int
	// Rebuild reprojects every candidate instead of only stale ones: after a
	// projection semantics change, a restore or a legacy import.
	Rebuild bool
}

// RepairResult reports one bounded repair step.
type RepairResult struct {
	Examined int
	Repaired int
	// Next is the cursor for the following call; nil when the range is done.
	Next *ProjectionKey
}

// RepairProjections is the owned projection repair. It examines up to Limit
// candidate keys in key order and rebuilds those whose state or daily
// projection is missing or older than their newest raw event (all of them with
// Rebuild). Idempotent; call again with After = Next until Next is nil.
func (st *Store) RepairProjections(ctx context.Context, tenant string, opts RepairOptions) (RepairResult, error) {
	var res RepairResult
	if strings.TrimSpace(tenant) == "" {
		return res, fmt.Errorf("signal: tenant is required")
	}
	if err := opts.Window.Validate(); err != nil {
		return res, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	var sb strings.Builder
	args := []any{tenant}
	fmt.Fprintf(&sb, `SELECT DISTINCT entity_type, entity_id, subject_kind, subject FROM %s.events WHERE tenant = ?`, st.db)
	pred, predArgs := opts.Window.predicate("occurred_at")
	sb.WriteString(pred)
	args = append(args, predArgs...)
	if !opts.IngestedSince.IsZero() {
		sb.WriteString(" AND ingested_at >= ?")
		args = append(args, opts.IngestedSince.UTC())
	}
	sb.WriteString(" AND " + st.notErased())
	if opts.After != nil {
		sb.WriteString(" AND (entity_type, entity_id, subject_kind, subject) > (?, ?, ?, ?)")
		args = append(args, opts.After.args()...)
	}
	sb.WriteString(" ORDER BY entity_type, entity_id, subject_kind, subject LIMIT ?")
	args = append(args, limit)
	keys, err := st.scanKeys(ctx, sb.String(), args...)
	if err != nil {
		return res, fmt.Errorf("signal: repair candidates: %w", err)
	}
	res.Examined = len(keys)
	if len(keys) == limit {
		last := keys[len(keys)-1]
		res.Next = &last
	}
	if len(keys) == 0 {
		return res, nil
	}
	repair := keys
	if !opts.Rebuild {
		if repair, err = st.staleKeys(ctx, tenant, keys); err != nil {
			return res, err
		}
	}
	if err := st.project(ctx, tenant, repair); err != nil {
		return res, err
	}
	res.Repaired = len(repair)
	return res, nil
}

func (st *Store) staleKeys(ctx context.Context, tenant string, keys []ProjectionKey) ([]ProjectionKey, error) {
	filter, keyArgs := keyFilter(keys)
	q := fmt.Sprintf(`SELECT r.entity_type, r.entity_id, r.subject_kind, r.subject
FROM (SELECT entity_type, entity_id, subject_kind, subject, max(ingested_at) AS raw
      FROM %[1]s.events WHERE tenant = ? AND %[2]s AND %[3]s GROUP BY entity_type, entity_id, subject_kind, subject) AS r
LEFT JOIN (SELECT entity_type, entity_id, subject_kind, subject, max(version) AS projected
      FROM %[1]s.subject_state WHERE tenant = ? AND %[2]s AND %[3]s GROUP BY entity_type, entity_id, subject_kind, subject) AS s
  USING (entity_type, entity_id, subject_kind, subject)
LEFT JOIN (SELECT entity_type, entity_id, subject_kind, subject, max(version) AS projected
      FROM %[1]s.subject_daily WHERE tenant = ? AND %[2]s AND %[3]s GROUP BY entity_type, entity_id, subject_kind, subject) AS d
  USING (entity_type, entity_id, subject_kind, subject)
WHERE s.projected < r.raw OR d.projected < r.raw
ORDER BY r.entity_type, r.entity_id, r.subject_kind, r.subject`, st.db, filter, st.notErased())
	args := make([]any, 0, 3+3*len(keyArgs))
	for i := 0; i < 3; i++ {
		args = append(args, tenant)
		args = append(args, keyArgs...)
	}
	keys, err := st.scanKeys(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: detect stale projections: %w", err)
	}
	return keys, nil
}

func (st *Store) scanKeys(ctx context.Context, q string, args ...any) ([]ProjectionKey, error) {
	rows, err := st.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectionKey
	for rows.Next() {
		var (
			k             ProjectionKey
			kind, subject string
		)
		if err := rows.Scan(&k.EntityType, &k.EntityID, &kind, &subject); err != nil {
			return nil, err
		}
		k.Subject = subjectFromKey(kind, subject)
		out = append(out, k)
	}
	return out, rows.Err()
}

// subjectFromKey rebuilds a Subject from its stored (kind, key) columns.
func subjectFromKey(kind, key string) Subject {
	if kind == SubjectKindUser {
		return Subject{UserID: key}
	}
	return Subject{AnonKey: key}
}

// Forget erases a subject's events and projections for one entity, or an
// entire entity type when entityID is empty. Windows and popularity read the
// per-subject projections, so they stop counting the subject at once. This is
// NOT account erasure: impressions and the item_pairs rollup remain, and a
// write racing the deletion can re-project.
func (st *Store) Forget(ctx context.Context, tenant string, subject Subject, entityType string, entityID string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(entityType) == "" {
		return fmt.Errorf("signal: entityType is required")
	}
	where := " WHERE tenant = ? AND entity_type = ? AND subject_kind = ? AND subject = ?"
	args := []any{tenant, entityType, subject.Kind(), subject.Key()}
	if strings.TrimSpace(entityID) != "" {
		where += " AND entity_id = ?"
		args = append(args, entityID)
	}
	for _, table := range []string{"events", "subject_state", "subject_daily"} {
		if err := st.conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s.%s%s", st.db, table, where), args...); err != nil {
			return fmt.Errorf("signal: forget %s: %w", table, err)
		}
	}
	return nil
}

// escapeCHString escapes a string for inline use in a ClickHouse literal.
func escapeCHString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
