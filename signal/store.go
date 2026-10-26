package signal

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Conn is the minimal ClickHouse surface the Store needs; clickhouse-go's
// driver.Conn satisfies it. Implementations must forward the context unchanged
// so clickhouse-go receives query options, including external tables.
type Conn interface {
	Exec(ctx context.Context, query string, args ...any) error
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// Store reads and writes the signal plane for one ClickHouse database.
// Every method is tenant-scoped via its tenant argument; every content
// reference it receives must belong to that tenant.
type Store struct {
	conn    Conn
	db      string
	cluster string
}

// StoreOptions identifies the complete, single-shard replica set that pooled
// connections may use. Cluster is needed for account anonymization when the
// signal tables have more than one replica.
type StoreOptions struct{ Cluster string }

// NewStore returns a Store over an existing ClickHouse connection. The schema
// comes from migrations.ClickHouse; verify it with CheckSchema.
func NewStore(conn Conn, database string, options ...StoreOptions) (*Store, error) {
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
	if len(options) > 1 {
		return nil, fmt.Errorf("signal: at most one store options value is allowed")
	}
	var cluster string
	if len(options) == 1 {
		cluster = strings.TrimSpace(options[0].Cluster)
		if cluster != "" && !identRe.MatchString(cluster) {
			return nil, fmt.Errorf("signal: invalid cluster name %q", cluster)
		}
	}
	return &Store{conn: conn, db: db, cluster: cluster}, nil
}

// refColumns are the content reference columns of every signal table.
const refColumns = "content_kind, content_id, content_version_id"

// ProjectionKey identifies one subject × content reference projection.
type ProjectionKey struct {
	ContentKey
	Subject Subject
}

func (k ProjectionKey) args() []any {
	return []any{k.ContentKind, k.ContentID, k.ContentVersionID, k.Subject.Kind(), k.Subject.Key()}
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

const projectionKeyFilter = "(" + refColumns + ", subject_kind, subject) IN " +
	"(SELECT " + refColumns + ", subject_kind, subject FROM contentkit_projection_keys)"

// Send keys as query-local data, not SQL literals repeated in each subquery.
func projectionKeyContext(ctx context.Context, keys []ProjectionKey) (context.Context, error) {
	table, err := ext.NewTable("contentkit_projection_keys",
		ext.Column("content_kind", "String"),
		ext.Column("content_id", "String"),
		ext.Column("content_version_id", "String"),
		ext.Column("subject_kind", "String"),
		ext.Column("subject", "String"),
	)
	if err != nil {
		return nil, fmt.Errorf("signal: projection keys: %w", err)
	}
	for _, k := range keys {
		if err := table.Append(
			k.ContentKind, k.ContentID, k.ContentVersionID,
			k.Subject.Kind(), k.Subject.Key(),
		); err != nil {
			return nil, fmt.Errorf("signal: projection keys: %w", err)
		}
	}
	return clickhouse.Context(ctx, clickhouse.WithExternalTable(table)), nil
}

// RecordSignals appends source events, then rebuilds the compact state and
// daily projections of every touched subject × content reference from its
// canonical events. Replays, reordering and revisions converge; nothing is
// incremented. Signals of erased subjects (EraseSubjects) are dropped. If the
// process stops between the insert and the projections, the events are durable
// and RepairProjections restores the projections.
// insertChunk bounds the rows of one inlined INSERT ... SELECT, and
// insertBytes the bytes those rows take once the driver inlines their
// arguments: ClickHouse rejects a statement over max_query_size (256 KiB by
// default), and the remainder covers the statement around the rows.
const (
	insertChunk = 200
	insertBytes = 240 << 10
)

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
	var measured, generic []Signal
	for _, s := range signals {
		if s.Measurement != nil || s.Type == TypeEngagement {
			measured = append(measured, s)
		} else {
			generic = append(generic, s)
		}
	}
	if len(measured) > 0 {
		if err := st.recordMeasured(ctx, tenant, measured); err != nil {
			return err
		}
		if len(generic) == 0 {
			return nil
		}
		signals = generic
	}
	subjects := make([]Subject, 0, len(signals))
	for i := range signals {
		if err := signals[i].validate(tenant); err != nil {
			return err
		}
		subjects = append(subjects, signals[i].Subject)
	}
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil {
		return err
	}
	const perRow = 12
	args := make([]any, 0, len(signals)*perRow)
	rows := make([]string, 0, len(signals))
	sizes := make([]int, 0, len(signals))
	for i := range signals {
		s := signals[i]
		if s.Type == "view" || s.Type == "preview" {
			return fmt.Errorf("signal: route-open signals are retired; use measured engagement")
		}
		if _, erased := fenced[[2]string{s.Subject.Kind(), s.Subject.Key()}]; erased {
			continue
		}
		payload, err := signalPayload(s)
		if err != nil {
			return err
		}
		// Use a server-side SELECT filter instead of a client-side fence check
		// alone. A request can be paused after fenced() and resume after an
		// erasure commits; evaluating the ledger in this statement prevents that
		// in-flight write from landing on any replica.
		rows = append(rows, "SELECT ? AS tenant, ? AS content_kind, ? AS content_id, ? AS content_version_id, ? AS subject_kind, ? AS subject, ? AS signal_type, ? AS event_id, ? AS revision, ? AS occurred_at, ? AS value, ? AS payload")
		args = append(args,
			tenant, s.ContentKind, s.ContentID, s.Version(), s.Subject.Kind(), s.Subject.Key(), s.Type,
			strings.TrimSpace(s.EventID), s.Revision, s.OccurredAt.UTC(),
			s.Value, payload,
		)
		sizes = append(sizes, inlinedSize(rows[len(rows)-1], args[len(args)-perRow:]))
	}
	if len(rows) == 0 {
		return nil
	}
	// Events are idempotent by event_id, so a retry after a partial insert
	// converges.
	for start, end := 0, 0; start < len(rows); start = end {
		end = insertEnd(sizes, start)
		insert := fmt.Sprintf(`INSERT INTO %s.signals
(tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id, revision, occurred_at,
 value, payload)
SELECT tenant, content_kind, content_id, content_version_id, subject_kind, subject, signal_type, event_id, revision, occurred_at,
       value, payload
FROM (%s) AS incoming
WHERE %s`, st.db, strings.Join(rows[start:end], " UNION ALL "), st.notErased())
		if err := st.conn.Exec(ctx, insert, args[start*perRow:end*perRow]...); err != nil {
			return fmt.Errorf("signal: insert signals: %w", err)
		}
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
// projection is missing or behind its canonical generation, or whose positive
// daily rows do not cover exactly the canonical days (all keys with Rebuild).
// Idempotent; call again with After = Next until Next is nil.
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
	fmt.Fprintf(&sb, `SELECT DISTINCT %s, subject_kind, subject FROM %s.engagement_segments WHERE tenant = ?`, refColumns, st.db)
	pred, predArgs := opts.Window.predicate("occurred_at")
	sb.WriteString(pred)
	args = append(args, predArgs...)
	if !opts.IngestedSince.IsZero() {
		sb.WriteString(" AND ingested_at >= ?")
		args = append(args, opts.IngestedSince.UTC())
	}
	sb.WriteString(" AND " + st.notErased())
	if opts.After != nil {
		sb.WriteString(" AND (" + refColumns + ", subject_kind, subject) > (?, ?, ?, ?, ?)")
		args = append(args, opts.After.args()...)
	}
	sb.WriteString(" ORDER BY " + refColumns + ", subject_kind, subject LIMIT ?")
	args = append(args, limit)
	keys, err := st.scanKeys(ctx, tenant, sb.String(), args...)
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
	if err := st.projectMeasured(ctx, tenant, repair); err != nil {
		return res, err
	}
	res.Repaired = len(repair)
	return res, nil
}

func (st *Store) staleKeys(ctx context.Context, tenant string, keys []ProjectionKey) ([]ProjectionKey, error) {
	ctx, err := projectionKeyContext(ctx, keys)
	if err != nil {
		return nil, err
	}
	const cols = refColumns + ", subject_kind, subject"

	q := fmt.Sprintf(`SELECT r.content_kind,r.content_id,r.content_version_id,r.subject_kind,r.subject
 FROM (SELECT %[4]s,sum(toUInt256(ver)+1) AS raw,groupUniqArray(toDate(c.1)) AS days FROM (%[5]s) GROUP BY %[4]s) r
 LEFT JOIN (SELECT %[4]s,max(version) AS projected FROM %[1]s.engagement_state WHERE tenant=? AND %[2]s AND %[3]s GROUP BY %[4]s) s USING(%[4]s)
 LEFT JOIN (SELECT %[4]s,minIf(version,present) AS projected,groupUniqArrayIf(day,present) AS days FROM %[1]s.engagement_daily FINAL
 WHERE tenant=? AND %[2]s AND %[3]s GROUP BY %[4]s) d USING(%[4]s)
 WHERE ifNull(s.projected,0)<r.raw OR ifNull(d.projected,0)<r.raw OR arraySort(ifNull(d.days,[]))!=arraySort(r.days)
 ORDER BY r.content_kind,r.content_id,r.content_version_id,r.subject_kind,r.subject`, st.db, projectionKeyFilter, st.notErased(), cols, st.measuredCanonical(projectionKeyFilter))

	keys, err = st.scanKeys(ctx, tenant, q, tenant, tenant, tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: detect stale projections: %w", err)
	}
	return keys, nil
}

func (st *Store) scanKeys(ctx context.Context, tenant, q string, args ...any) ([]ProjectionKey, error) {
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
		k.TenantID = tenant
		if err := rows.Scan(&k.ContentKind, &k.ContentID, &k.ContentVersionID, &kind, &subject); err != nil {
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

// Forget erases a subject's events and projections for one content item (the
// work and all its versions) or an entire content kind when contentID is
// empty. Windows and popularity read the per-subject projections, so they stop
// counting the subject at once. This is NOT account erasure: exposures and the
// content_pairs rollup remain, and a write racing the deletion can re-project.
func (st *Store) Forget(ctx context.Context, tenant string, subject Subject, contentKind string, contentID string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	if err := subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(contentKind) == "" {
		return fmt.Errorf("signal: contentKind is required")
	}
	where := " WHERE tenant = ? AND content_kind = ? AND subject_kind = ? AND subject = ?"
	args := []any{tenant, contentKind, subject.Kind(), subject.Key()}
	if strings.TrimSpace(contentID) != "" {
		where += " AND content_id = ?"
		args = append(args, contentID)
	}
	for _, table := range []string{"engagement_segments", "engagement_state", "engagement_daily", "legacy_reading_state"} {
		if err := st.conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s.%s%s", st.db, table, where), args...); err != nil {
			return fmt.Errorf("signal: forget %s: %w", table, err)
		}
	}
	return nil
}

// ForgottenContent names purged content: a whole work (VersionID empty) or
// one version of it.
type ForgottenContent struct {
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

// contentTables hold per-content rows; exposures keep their served lists.
var contentTables = []string{"signals", "subject_content_state", "subject_content_daily", "engagement_segments",
	"engagement_state", "engagement_daily", "legacy_reading_state"}

// ForgetContents erases every subject's events and projections of purged
// content, and a purged work's co-engagement pairs. Idempotent.
func (st *Store) ForgetContents(ctx context.Context, tenant string, contents []ForgottenContent) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("signal: tenant is required")
	}
	var works, versions [][]any
	for _, c := range contents {
		if strings.TrimSpace(c.Kind) == "" || strings.TrimSpace(c.ContentID) == "" {
			return fmt.Errorf("signal: forgotten content needs a kind and an id")
		}
		if c.VersionID == "" {
			works = append(works, []any{c.Kind, c.ContentID})
		} else {
			versions = append(versions, []any{c.Kind, c.ContentID, c.VersionID})
		}
	}
	active, err := st.activeSubjectTables(ctx)
	if err != nil {
		return err
	}
	var tables []string
	for _, t := range contentTables {
		if slices.Contains(active, t) {
			tables = append(tables, t)
		}
	}
	pairs, err := st.existing(ctx, []string{"engagement_pairs", "content_pairs"})
	if err != nil {
		return err
	}
	forget := func(rows [][]any, tables []string, where func(in string) string) error {
		for start := 0; start < len(rows); start += mutationTupleChunk {
			chunk := rows[start:min(start+mutationTupleChunk, len(rows))]
			marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(chunk[0])), ", ") + ")"
			tuples := make([]string, len(chunk))
			args := []any{tenant}
			for i, r := range chunk {
				tuples[i] = marks
				args = append(args, r...)
			}
			in := "(" + strings.Join(tuples, ", ") + ")"
			cond := where(in)
			if strings.Count(cond, in) == 2 {
				args = append(args, args[1:]...)
			}
			for _, table := range tables {
				if err := st.mutate(ctx, table, "tenant = ? AND "+cond, args...); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := forget(works, tables, func(in string) string { return "(content_kind, content_id) IN " + in }); err != nil {
		return err
	}
	if err := forget(works, pairs, func(in string) string {
		return fmt.Sprintf("((content_kind_a, content_id_a) IN %[1]s OR (content_kind_b, content_id_b) IN %[1]s)", in)
	}); err != nil {
		return err
	}
	return forget(versions, tables, func(in string) string { return "(content_kind, content_id, content_version_id) IN " + in })
}

// inlinedSize bounds the bytes a row's SELECT takes once the driver inlines
// args into it: quoting and escaping at most double a string plus its
// separator, and no other value renders in more than 64 bytes.
func inlinedSize(row string, args []any) int {
	n := len(row)
	for _, a := range args {
		switch v := a.(type) {
		case string:
			n += 2*len(v) + 4
		case []string:
			for _, s := range v {
				n += 2*len(s) + 4
			}
		case []uint32:
			n += 12*len(v) + 2
		default:
			n += 64
		}
	}
	return n
}

// insertEnd returns the end of the INSERT whose rows start at sizes[start]: at
// most insertChunk rows and, past the first, insertBytes.
func insertEnd(sizes []int, start int) int {
	end, total := start, 0
	for end < len(sizes) && end-start < insertChunk && (end == start || total+sizes[end] <= insertBytes) {
		total += sizes[end]
		end++
	}
	return end
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
