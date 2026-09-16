package signal

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// subjectHash is the fence key: sipHash128 over "kind:subject".
const subjectHashExpr = "sipHash128(concat(subject_kind, ':', subject))"

// ErasureReport describes one erasure or enforcement pass.
type ErasureReport struct {
	// Remaining counts rows still attributable to the subjects per table after
	// the pass; every value is zero when erasure is complete.
	Remaining map[string]uint64
	// PairsRemoved counts item_pairs rows invalidated because an erased subject
	// contributed to an entity in them; RefreshCoEngagement rebuilds them.
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
var subjectTables = []string{"events", "subject_state", "subject_daily", "exposures"}

// EraseSubjects permanently erases subjects from every listed tenant: it
// records the fence first (so concurrent and later writes, impressions and
// projection rebuilds for them are dropped), deletes their events, compact
// state, daily contributions, impressions and legacy raw rows, removes
// co-engagement pairs touching entities they contributed to, waits for the
// mutations on every replica and verifies nothing remains. Idempotent. After
// restoring a backup, replay every erasure newer than the backup and run
// EnforceErasures.
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
	for _, tenant := range tenants {
		fence := fmt.Sprintf(`INSERT INTO %s.erasures (tenant, subject_hash, erased_at)
SELECT ?, sipHash128(concat(t.1, ':', t.2)), now64(6) FROM (SELECT arrayJoin(arrayZip(?, ?)) AS t)`, st.db)
		if err := st.conn.Exec(ctx, fence, tenant, kinds, keys); err != nil {
			return report, fmt.Errorf("signal: record erasure fence: %w", err)
		}
	}
	for _, tenant := range tenants {
		pass, err := st.eraseWhere(ctx, tenant, filter, args)
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

// EnforceOptions bounds EnforceErasures.
type EnforceOptions struct {
	// Since limits enforcement to erasures recorded at or after it (zero = all).
	// Periodic runs pass the previous run's start; a restore passes zero.
	Since time.Time
}

// EnforceErasures re-applies recorded erasures of a tenant: it deletes residue
// written by writers that passed the fence check before the fence existed, or
// rows restored from a backup. Idempotent; schedule it and run it with a zero
// Since after every restore (after re-erasing subjects deleted since the
// backup, which the host's own deletion ledger knows).
func (st *Store) EnforceErasures(ctx context.Context, tenant string, opts EnforceOptions) (ErasureReport, error) {
	report := ErasureReport{Remaining: map[string]uint64{}}
	if strings.TrimSpace(tenant) == "" {
		return report, fmt.Errorf("signal: tenant is required")
	}
	q := fmt.Sprintf("SELECT DISTINCT hex(subject_hash) FROM %s.erasures WHERE tenant = ?", st.db)
	args := []any{tenant}
	if !opts.Since.IsZero() {
		q += " AND erased_at >= ?"
		args = append(args, opts.Since.UTC())
	}
	rows, err := st.conn.Query(ctx, q+" ORDER BY 1", args...)
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
		pass, err := st.eraseWhere(ctx, tenant, filter, []any{chunk})
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

func (st *Store) eraseWhere(ctx context.Context, tenant, filter string, args []any) (ErasureReport, error) {
	report := ErasureReport{Remaining: map[string]uint64{}}
	tables := append([]string{}, subjectTables...)
	for _, legacy := range []string{"signal_events", "search_impressions"} {
		exists, err := st.tableExists(ctx, legacy)
		if err != nil {
			return report, err
		}
		if exists {
			tables = append(tables, legacy)
		}
	}
	// Pairs first: the contributed entity set is read from the rows being
	// erased, and passed as literals so every replica deletes the same pairs.
	entities, err := st.contributedEntities(ctx, tenant, filter, args)
	if err != nil {
		return report, err
	}
	for start := 0; start < len(entities); start += mutationTupleChunk {
		chunk := entities[start:min(start+mutationTupleChunk, len(entities))]
		tuples := make([]string, len(chunk))
		pairArgs := make([]any, 0, 1+4*len(chunk))
		pairArgs = append(pairArgs, tenant)
		for i, e := range chunk {
			tuples[i] = "(?, ?)"
			pairArgs = append(pairArgs, e.EntityType, e.EntityID)
		}
		in := "(" + strings.Join(tuples, ", ") + ")"
		pairArgs = append(pairArgs, pairArgs[1:]...)
		where := fmt.Sprintf("tenant = ? AND ((entity_type_a, entity_id_a) IN %[1]s OR (entity_type_b, entity_id_b) IN %[1]s)", in)
		n, err := st.count(ctx, "item_pairs", tenant, where[len("tenant = ? AND "):], pairArgs[1:])
		if err != nil {
			return report, err
		}
		if n == 0 {
			continue
		}
		if err := st.mutate(ctx, "item_pairs", where, pairArgs...); err != nil {
			return report, err
		}
		report.PairsRemoved += n
	}
	for _, table := range tables {
		if err := st.mutate(ctx, table, "tenant = ? AND "+filter, append([]any{tenant}, args...)...); err != nil {
			return report, err
		}
	}
	for _, table := range tables {
		n, err := st.count(ctx, table, tenant, filter, args)
		if err != nil {
			return report, err
		}
		report.Remaining[tenant+"."+table] = n
	}
	return report, nil
}

// mutationTupleChunk bounds the literal IN list of one pair deletion.
const mutationTupleChunk = 1000

// contributedEntities lists entities the filtered subjects have daily rows for.
func (st *Store) contributedEntities(ctx context.Context, tenant, filter string, args []any) ([]EntityRef, error) {
	q := fmt.Sprintf(`SELECT DISTINCT entity_type, entity_id FROM %s.subject_daily WHERE tenant = ? AND %s ORDER BY entity_type, entity_id`, st.db, filter)
	rows, err := st.conn.Query(ctx, q, append([]any{tenant}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("signal: contributed entities: %w", err)
	}
	defer rows.Close()
	var out []EntityRef
	for rows.Next() {
		var ref EntityRef
		if err := rows.Scan(&ref.EntityType, &ref.EntityID); err != nil {
			return nil, err
		}
		out = append(out, ref)
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

func (st *Store) tableExists(ctx context.Context, table string) (bool, error) {
	rows, err := st.conn.Query(ctx, "SELECT count() FROM system.tables WHERE database = ? AND name = ?", st.db, table)
	if err != nil {
		return false, fmt.Errorf("signal: table lookup: %w", err)
	}
	defer rows.Close()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
	}
	return n > 0, rows.Err()
}

func subjectColumns(subjects []Subject) (kinds, keys []string) {
	for _, s := range subjects {
		kinds = append(kinds, s.Kind())
		keys = append(keys, s.Key())
	}
	return kinds, keys
}

// fenced returns the subjects among the given ones that a recorded erasure
// covers; writes for them are dropped.
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
