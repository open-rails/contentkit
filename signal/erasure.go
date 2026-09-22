package signal

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// subjectHashExpr is the fence key: sipHash128 over "kind:subject".
const subjectHashExpr = "sipHash128(concat(subject_kind, ':', subject))"

// subjectHashKey is the Go-side input of the same hash for one subject.
func subjectHashKey(s Subject) string { return s.Kind() + ":" + s.Key() }

// notErased is the read and write barrier for subject-bearing rows. It is
// deliberately evaluated by ClickHouse in the same INSERT/SELECT statement
// as the data operation: a writer that read the ledger before EraseSubjects
// recorded its fence is still filtered after the fence commits. Every read,
// projection and export of subject rows carries it (or subjectNotErased).
func (st *Store) notErased() string {
	return "(" + subjectHashExpr + ", tenant) NOT IN (SELECT subject_hash, tenant FROM " + st.db + ".erasures)"
}

// subjectNotErased is the barrier for single-subject reads: a primary-key
// lookup of the ledger evaluated once per query instead of materializing the
// whole ledger. Binds two arguments: the tenant and subjectHashKey(subject).
func (st *Store) subjectNotErased() string {
	return "(SELECT count() FROM " + st.db + ".erasures WHERE tenant = ? AND subject_hash = sipHash128(?)) = 0"
}

// ErasureReport describes one erasure or enforcement pass.
type ErasureReport struct {
	// Remaining counts rows still attributable to the subjects per table after
	// the pass; every value is zero when erasure is complete.
	Remaining map[string]uint64
	// PairsRemoved counts content_pairs rows invalidated because an erased
	// subject contributed to a work in them; RefreshCoEngagement rebuilds them.
	PairsRemoved uint64
}

// Complete reports whether no attributable rows remain.
func (r ErasureReport) Complete() bool {
	for _, n := range r.Remaining {
		if n > 0 {
			return false
		}
	}
	return true
}

// subjectTables hold rows keyed by (tenant, subject_kind, subject).
var subjectTables = []string{"signals", "subject_content_state", "subject_content_daily", "exposures"}

// maxErasurePasses bounds the delete-and-verify loop of one erasure: a row
// landing between a pass's mutation and its count is deleted by the next pass.
const maxErasurePasses = 3

// EraseSubjects permanently erases subjects from every listed tenant.
//
// Completion contract: when the returned report is Complete(),
//
//  1. the erasure is recorded in the ledger on every replica (quorum insert),
//     so from that moment every write, read, projection and export on any
//     replica drops or hides the subjects' rows (the barrier): the subject
//     key is dead in the tenant forever;
//  2. every row of the subjects that existed when the pass ran is deleted from
//     signals, compact state, daily contributions and exposures on every replica (mutations_sync = 2) and re-counted as zero;
//  3. co-engagement pairs touching works the subjects contributed to are
//     removed (RefreshCoEngagement rebuilds them and re-verifies the ledger
//     around its build).
//
// A writer or projection that observed the pre-erasure state can still leave
// residue rows; they are unreadable by (1) and EnforceErasures removes them
// physically. Idempotent. Returns an error without Complete() when a replica
// is unavailable (the fence would not be durable everywhere) or rows remain
// after maxErasurePasses. After restoring a backup, replay every erasure newer
// than the backup and run EnforceErasures.
func (st *Store) EraseSubjects(ctx context.Context, tenants []string, subjects []Subject) (ErasureReport, error) {
	report := ErasureReport{Remaining: map[string]uint64{}}
	tenants = trimAll(tenants)
	if len(tenants) == 0 {
		return report, fmt.Errorf("signal: tenants are required")
	}
	if len(subjects) == 0 {
		return report, nil
	}
	if len(subjects) > MaxErasureSubjects {
		return report, &LimitError{Field: "subjects per erasure", Limit: MaxErasureSubjects, Got: len(subjects)}
	}
	filter, args, err := subjectFilter(subjects)
	if err != nil {
		return report, err
	}
	kinds, keys := subjectColumns(subjects)
	quorum, err := st.erasureQuorum(ctx)
	if err != nil {
		return report, err
	}
	for _, tenant := range tenants {
		fence := fmt.Sprintf(`INSERT INTO %s.erasures (tenant, subject_hash, erased_at)
	SELECT ?, sipHash128(concat(t.1, ':', t.2)), now64(6) FROM (SELECT arrayJoin(arrayZip(?, ?)) AS t)%s`, st.db, quorumSetting(quorum))
		if err := st.conn.Exec(ctx, fence, tenant, kinds, keys); err != nil {
			return report, fmt.Errorf("signal: record erasure fence: %w", err)
		}
	}
	for _, tenant := range tenants {
		pass, err := st.erasePasses(ctx, tenant, filter, args)
		if err != nil {
			return report, err
		}
		report.merge(pass)
	}
	if !report.Complete() {
		return report, fmt.Errorf("signal: erasure incomplete: %v", report.Remaining)
	}
	return report, nil
}

// erasedCount is the ledger's distinct subject count: RefreshCoEngagement
// re-verifies it around a build so a rollup never publishes a subject erased
// while it was being computed.
func (st *Store) erasedCount(ctx context.Context, tenant string) (uint64, error) {
	rows, err := st.conn.Query(ctx, fmt.Sprintf("SELECT uniqExact(subject_hash) FROM %s.erasures WHERE tenant = ?", st.db), tenant)
	if err != nil {
		return 0, fmt.Errorf("signal: erasure ledger: %w", err)
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// EnforceErasures re-applies every recorded erasure of a tenant to every row:
// it deletes residue left by writers or projections that observed the state
// before the fence, or rows restored from a backup. Such rows were already
// unreadable; this removes them physically. There is no cursor, so a delayed
// write for an old erasure is never skipped. Idempotent; schedule it, and run
// it after every restore (after re-erasing subjects deleted since the backup,
// which the host's own deletion ledger knows).
func (st *Store) EnforceErasures(ctx context.Context, tenant string) (ErasureReport, error) {
	report := ErasureReport{Remaining: map[string]uint64{}}
	if strings.TrimSpace(tenant) == "" {
		return report, fmt.Errorf("signal: tenant is required")
	}
	q := fmt.Sprintf("SELECT DISTINCT hex(subject_hash) FROM %s.erasures WHERE tenant = ? ORDER BY 1", st.db)
	rows, err := st.conn.Query(ctx, q, tenant)
	if err != nil {
		return report, fmt.Errorf("signal: read erasures: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return report, err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	for start := 0; start < len(hashes); start += mutationTupleChunk {
		chunk := hashes[start:min(start+mutationTupleChunk, len(hashes))]
		filter := "hex(" + subjectHashExpr + ") IN ?"
		pass, err := st.erasePasses(ctx, tenant, filter, []any{chunk})
		if err != nil {
			return report, err
		}
		report.merge(pass)
	}
	if !report.Complete() {
		return report, fmt.Errorf("signal: erasure enforcement incomplete: %v", report.Remaining)
	}
	return report, nil
}

// erasePasses runs eraseWhere until nothing remains or maxErasurePasses.
func (st *Store) erasePasses(ctx context.Context, tenant, filter string, args []any) (ErasureReport, error) {
	var pairs uint64
	for pass := 1; ; pass++ {
		report, err := st.eraseWhere(ctx, tenant, filter, args)
		if err != nil {
			return report, err
		}
		pairs += report.PairsRemoved
		report.PairsRemoved = pairs
		if report.Complete() || pass == maxErasurePasses {
			return report, nil
		}
	}
}

func (st *Store) eraseWhere(ctx context.Context, tenant, filter string, args []any) (ErasureReport, error) {
	report := ErasureReport{Remaining: map[string]uint64{}}
	// Pairs first: the contributed work set is read from the rows being
	// erased, and passed as literals so every replica deletes the same pairs
	// (replicated mutations reject subqueries as nondeterministic).
	works, err := st.contributedContent(ctx, tenant, filter, args)
	if err != nil {
		return report, err
	}
	for start := 0; start < len(works); start += mutationTupleChunk {
		chunk := works[start:min(start+mutationTupleChunk, len(works))]
		tuples := make([]string, len(chunk))
		pairArgs := make([]any, 0, 1+4*len(chunk))
		pairArgs = append(pairArgs, tenant)
		for i, w := range chunk {
			tuples[i] = "(?, ?)"
			pairArgs = append(pairArgs, w[0], w[1])
		}
		in := "(" + strings.Join(tuples, ", ") + ")"
		pairArgs = append(pairArgs, pairArgs[1:]...)
		where := fmt.Sprintf("tenant = ? AND ((content_kind_a, content_id_a) IN %[1]s OR (content_kind_b, content_id_b) IN %[1]s)", in)
		n, err := st.count(ctx, "content_pairs", tenant, where[len("tenant = ? AND "):], pairArgs[1:])
		if err != nil {
			return report, err
		}
		if n == 0 {
			continue
		}
		if err := st.mutate(ctx, "content_pairs", where, pairArgs...); err != nil {
			return report, err
		}
		report.PairsRemoved += n
	}
	for _, table := range subjectTables {
		if err := st.mutate(ctx, table, "tenant = ? AND "+filter, append([]any{tenant}, args...)...); err != nil {
			return report, err
		}
	}
	for _, table := range subjectTables {
		n, err := st.count(ctx, table, tenant, filter, args)
		if err != nil {
			return report, err
		}
		report.Remaining[tenant+"."+table] = n
	}
	return report, nil
}

// mutationTupleChunk bounds the literal IN list of one mutation.
const mutationTupleChunk = 1000

// contributedContent lists the works (kind, id) the filtered subjects have
// daily rows for; version rows collapse onto their work.
func (st *Store) contributedContent(ctx context.Context, tenant, filter string, args []any) ([][2]string, error) {
	q := fmt.Sprintf(`SELECT DISTINCT content_kind, content_id FROM %s.subject_content_daily WHERE tenant = ? AND %s ORDER BY content_kind, content_id`, st.db, filter)
	rows, err := st.conn.Query(ctx, q, append([]any{tenant}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("signal: contributed content: %w", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var w [2]string
		if err := rows.Scan(&w[0], &w[1]); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *ErasureReport) merge(o ErasureReport) {
	for k, v := range o.Remaining {
		r.Remaining[k] += v
	}
	r.PairsRemoved += o.PairsRemoved
}

func (st *Store) count(ctx context.Context, table, tenant, filter string, args []any) (uint64, error) {
	q := fmt.Sprintf("SELECT count() FROM %s.%s WHERE tenant = ? AND (%s)", st.db, table, filter)
	rows, err := st.conn.Query(ctx, q, append([]any{tenant}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("signal: count %s: %w", table, err)
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

func subjectColumns(subjects []Subject) (kinds, keys []string) {
	for _, s := range subjects {
		kinds = append(kinds, s.Kind())
		keys = append(keys, s.Key())
	}
	return kinds, keys
}

// erasureQuorum is the number of replicas Keeper expects for the erasure
// ledger. A quorum insert makes EraseSubjects fail closed when a replica is
// unavailable instead of reporting completion while another process can still
// read the subject as live.
func (st *Store) erasureQuorum(ctx context.Context) (uint32, error) {
	rows, err := st.conn.Query(ctx, "SELECT max(toUInt32(total_replicas)) FROM system.replicas WHERE database = ? AND table = 'erasures'", st.db)
	if err != nil {
		return 0, fmt.Errorf("signal: erasure quorum: %w", err)
	}
	defer rows.Close()
	var n uint32
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, fmt.Errorf("signal: erasure quorum scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		n = 1
	}
	return n, nil
}

func quorumSetting(quorum uint32) string {
	if quorum <= 1 {
		return ""
	}
	return fmt.Sprintf(" SETTINGS insert_quorum = %d, insert_quorum_parallel = 1", quorum)
}

// ErasedSubjects reports which of the given subjects a recorded erasure of
// tenant covers, so a producer can retire their obligations instead of
// retrying deliveries the ledger will drop.
func (st *Store) ErasedSubjects(ctx context.Context, tenant string, subjects []Subject) (map[Subject]bool, error) {
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil {
		return nil, err
	}
	out := make(map[Subject]bool, len(fenced))
	for k := range fenced {
		out[subjectFromKey(k[0], k[1])] = true
	}
	return out, nil
}

// fenced returns the subjects among the given ones that a recorded erasure
// covers; writers skip them before building their INSERT. This is hygiene,
// not the barrier: the INSERT itself and every read re-evaluate the ledger.
func (st *Store) fenced(ctx context.Context, tenant string, subjects []Subject) (map[[2]string]struct{}, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	kinds, keys := subjectColumns(subjects)
	q := fmt.Sprintf(`SELECT t.1, t.2 FROM (SELECT arrayJoin(arrayZip(?, ?)) AS t)
WHERE sipHash128(concat(t.1, ':', t.2)) IN (SELECT subject_hash FROM %s.erasures WHERE tenant = ?)`, st.db)
	rows, err := st.conn.Query(ctx, q, kinds, keys, tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: erasure fence: %w", err)
	}
	defer rows.Close()
	out := map[[2]string]struct{}{}
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	return out, rows.Err()
}

// subjectFilter renders "(subject_kind, subject) IN (...)" for valid, deduplicated subjects.
func subjectFilter(subjects []Subject) (string, []any, error) {
	seen := map[[2]string]struct{}{}
	for _, s := range subjects {
		if err := s.Validate(); err != nil {
			return "", nil, err
		}
		seen[[2]string{s.Kind(), s.Key()}] = struct{}{}
	}
	keys := make([][2]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	tuples := make([]string, len(keys))
	args := make([]any, 0, 2*len(keys))
	for i, k := range keys {
		tuples[i] = "(?, ?)"
		args = append(args, k[0], k[1])
	}
	return "(subject_kind, subject) IN (" + strings.Join(tuples, ", ") + ")", args, nil
}
