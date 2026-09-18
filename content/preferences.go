package content

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// The preference boundary is the durable, ordered projection of an actor's
// current preference per (canonical content reference, axis): like/dislike/
// neutral on the reaction axis, favorited/not on the favorite axis.
//
// Options.Canonicalizer maps a resolved reference to the one reference its
// preference is recorded under (per-language routes collapse to one
// content_id; an explicit version stays a distinct key), and that reference is
// what the reaction/favorite row, the counts rollup and the snapshot all use.
// Comment threads keep their own localized reference.
//
// Each preference-bearing mutation takes a transaction advisory lock on its
// key, applies the social mutation and its counters, then allocates a
// revision from the schema-local sequence with clock_timestamp() — all in the
// caller's transaction. A rolled-back mutation leaves no exportable snapshot;
// a no-op allocates no revision.

// Preference axes stored in the snapshot table.
const (
	PreferenceAxisReaction = "reaction" // value -1 dislike / 0 neutral / 1 like
	PreferenceAxisFavorite = "favorite" // value 1 favorited / 0 not
)

// maxPreferenceRevisionFloor bounds a seeded revision floor well inside bigint.
const maxPreferenceRevisionFloor = math.MaxInt64 / 2

// PreferenceKey identifies one snapshot: the tenant, an authenticated actor,
// the canonical content reference and the axis. Field order is the table's
// key order (keyset cursors compare it).
type PreferenceKey struct {
	TenantID         string
	ActorID          string
	ContentKind      string
	ContentID        string
	ContentVersionID string
	Axis             string
}

// Ref returns the snapshot's content reference.
func (k PreferenceKey) Ref() contentref.ContentRef {
	return contentref.NewVersion(k.TenantID, k.ContentKind, k.ContentID, k.ContentVersionID)
}

func (k PreferenceKey) args() []any {
	return []any{k.TenantID, k.ActorID, k.ContentKind, k.ContentID, k.ContentVersionID, k.Axis}
}

// PreferenceSnapshot is the immutable copy of one committed preference: the
// actor's current value, the revision that committed it and that revision's
// mutation time. DeliveredRevision is the highest acknowledged revision.
type PreferenceSnapshot struct {
	PreferenceKey
	Value             int16
	Revision          int64
	OccurredAt        time.Time
	DeliveredRevision int64
}

// Pending reports whether the snapshot still owes a delivery.
func (s PreferenceSnapshot) Pending() bool { return s.DeliveredRevision < s.Revision }

// PreferenceAck acknowledges delivery of exactly Revision for the key.
type PreferenceAck struct {
	PreferenceKey
	Revision int64
}

// preferences owns the snapshot writes and reads. canon is the host's
// canonical-reference rule; nil means the host does not export preferences.
type preferences struct {
	rt    *Runtime
	s     *store
	canon ContentCanonicalizer
}

func newPreferences(rt *Runtime, canon ContentCanonicalizer) *preferences {
	return &preferences{rt: rt, s: rt.store, canon: canon}
}

// work resolves the canonical preference reference of a target; ok is false
// when preferences are disabled or the host declines the target, which also
// means the target keeps its own storage reference.
func (p *preferences) work(ref contentref.ContentRef) (contentref.ContentRef, bool) {
	if p.canon == nil || ref.ContentKind == KindComment {
		return ref, false
	}
	w, ok := p.canon.Canonical(ref)
	if !ok || w.ContentID == "" {
		return ref, false
	}
	if w.TenantID == "" {
		w.TenantID = p.s.tenant
	}
	if w.ContentKind == "" {
		w.ContentKind = ref.ContentKind
	}
	if p.rt.checkRef(w) != nil {
		return ref, false
	}
	return w, true
}

// target resolves one mutation: the reference the source row is stored under
// (canonical, or the caller's for a declined target) and the snapshot key.
// exportable is false when preferences are disabled, the host declined the
// target, or the actor has no durable subject: an anonymous IP identity is
// never an analytics subject.
func (p *preferences) target(actor Actor, ref contentref.ContentRef, axis string) (storage contentref.ContentRef, key PreferenceKey, exportable bool) {
	w, ok := p.work(ref)
	if !ok || actor.Anonymous || actor.ID == "" {
		return w, PreferenceKey{}, false
	}
	return w, PreferenceKey{TenantID: w.TenantID, ActorID: actor.ID, ContentKind: w.ContentKind, ContentID: w.ContentID, ContentVersionID: w.Version(), Axis: axis}, true
}

// storedRefSet maps caller references to the references their rows are stored
// under, and back.
type storedRefSet struct {
	refs    []contentref.ContentRef
	callers map[contentref.ContentKey][]contentref.ContentKey
}

// storedRefs validates the caller's references and resolves each through the
// canonical rule, so own-state reads agree with the writes.
func (p *preferences) storedRefs(refs []contentref.ContentRef) (storedRefSet, error) {
	out := storedRefSet{refs: make([]contentref.ContentRef, 0, len(refs)), callers: make(map[contentref.ContentKey][]contentref.ContentKey, len(refs))}
	for _, r := range refs {
		if err := p.rt.checkRef(r); err != nil {
			return storedRefSet{}, err
		}
		w, _ := p.work(r)
		out.refs = append(out.refs, w)
		out.callers[w.Key()] = append(out.callers[w.Key()], r.Key())
	}
	return out, nil
}

const preferenceCols = `tenant_id, actor_id, content_kind, content_id, content_version_id, axis, value, revision, occurred_at, delivered_revision`

func scanPreference(row pgx.Row) (PreferenceSnapshot, error) {
	var s PreferenceSnapshot
	err := row.Scan(&s.TenantID, &s.ActorID, &s.ContentKind, &s.ContentID, &s.ContentVersionID, &s.Axis, &s.Value, &s.Revision, &s.OccurredAt, &s.DeliveredRevision)
	return s, err
}

// lock serializes one key for the rest of the caller's transaction, before
// any revision is allocated. Hash collisions only serialize unrelated keys.
func (p *preferences) lock(ctx context.Context, tx pgx.Tx, key PreferenceKey) error {
	parts := []string{p.s.schema, key.TenantID, key.ActorID, key.ContentKind, key.ContentID, key.ContentVersionID, key.Axis}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, strings.Join(parts, "\x1f"))
	return err
}

// record writes the snapshot for a committed transition inside the mutation's
// transaction: a fresh sequence revision and clock_timestamp(), taken after the
// key lock the caller already holds.
func (p *preferences) record(ctx context.Context, tx pgx.Tx, key PreferenceKey, value int16) (*PreferenceSnapshot, error) {
	snap, err := scanPreference(tx.QueryRow(ctx, `INSERT INTO `+p.s.t.preferenceSnapshots+` AS t
		(tenant_id, actor_id, content_kind, content_id, content_version_id, axis, value, revision, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, nextval('`+p.s.revisionSeq+`'), clock_timestamp())
		ON CONFLICT (tenant_id, actor_id, content_kind, content_id, content_version_id, axis) DO UPDATE
		SET value = EXCLUDED.value, revision = EXCLUDED.revision, occurred_at = EXCLUDED.occurred_at
		RETURNING `+preferenceCols, append(key.args(), value)...))
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// mutate runs one preference-bearing mutation in tx: lock the key, apply the
// social change through apply (which reports whether state changed), then
// record the snapshot. Nothing is exported for a no-op or a declined target.
func (p *preferences) mutate(ctx context.Context, tx pgx.Tx, key PreferenceKey, exportable bool, value int16, apply func() (bool, error)) (*PreferenceSnapshot, error) {
	if exportable {
		if err := p.lock(ctx, tx, key); err != nil {
			return nil, err
		}
	}
	changed, err := apply()
	if err != nil || !changed || !exportable {
		return nil, err
	}
	return p.record(ctx, tx, key, value)
}

// PendingPreferences pages snapshots of this tenant that still owe a delivery
// (delivered_revision < revision) in key order; pass the last row's key as
// `after` to continue the sweep (zero value = from the start). The cursor is
// per-sweep only: the next sweep starts from the pending rows again, so one key
// that keeps failing delays nothing else. A retry re-reads the same revision
// and occurred_at.
func (rt *Runtime) PendingPreferences(ctx context.Context, after PreferenceKey, limit int) ([]PreferenceSnapshot, error) {
	return rt.preferences.scan(ctx, after, limit, true)
}

// ScanPreferences pages the tenant's whole snapshot table in key order,
// zero-valued removals and already-delivered rows included: the replay and
// reseed authority, reading exactly what live delivery reads.
func (rt *Runtime) ScanPreferences(ctx context.Context, after PreferenceKey, limit int) ([]PreferenceSnapshot, error) {
	return rt.preferences.scan(ctx, after, limit, false)
}

func (p *preferences) scan(ctx context.Context, after PreferenceKey, limit int, pendingOnly bool) ([]PreferenceSnapshot, error) {
	if limit <= 0 {
		limit = 100
	}
	pred := ""
	if pendingOnly {
		pred = ` AND delivered_revision < revision`
	}
	rows, err := p.s.pool.Query(ctx, `SELECT `+preferenceCols+` FROM `+p.s.t.preferenceSnapshots+` AS snapshot
		WHERE NOT EXISTS (SELECT 1 FROM `+p.rt.privateFences()+` f WHERE f.tenant_id=snapshot.tenant_id AND f.actor_id=snapshot.actor_id) AND tenant_id = $1 AND (actor_id, content_kind, content_id, content_version_id, axis) > ($2, $3, $4, $5, $6)`+pred+`
		ORDER BY actor_id, content_kind, content_id, content_version_id, axis
		LIMIT $7`, p.s.tenant, after.ActorID, after.ContentKind, after.ContentID, after.ContentVersionID, after.Axis, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PreferenceSnapshot, 0, min(limit, 128))
	for rows.Next() {
		s, err := scanPreference(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AcknowledgePreferences records delivery of exactly the revisions sent (batch
// shaped). A key mutated since stays pending (GREATEST guarded by
// sent <= revision), an older ack never lowers a higher one, and an ack naming
// a revision no snapshot of this tenant holds is an error: it can only come
// from a sink bug. Acknowledgement never touches value/revision/occurred_at.
func (rt *Runtime) AcknowledgePreferences(ctx context.Context, acks []PreferenceAck) error {
	if len(acks) == 0 {
		return nil
	}
	byKey := make(map[PreferenceKey]int64, len(acks))
	for _, a := range acks {
		if a.Revision < 1 {
			return fmt.Errorf("content: preference ack for %+v needs a revision >= 1", a.PreferenceKey)
		}
		if a.TenantID != rt.tenant {
			return ErrTenant
		}
		if a.Revision > byKey[a.PreferenceKey] {
			byKey[a.PreferenceKey] = a.Revision
		}
	}
	n := len(byKey)
	actors, kinds, ids, versions, axes := make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]string, 0, n)
	revs := make([]int64, 0, n)
	for k, rev := range byKey {
		actors, kinds, ids, versions, axes = append(actors, k.ActorID), append(kinds, k.ContentKind), append(ids, k.ContentID), append(versions, k.ContentVersionID), append(axes, k.Axis)
		revs = append(revs, rev)
	}
	tag, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.preferenceSnapshots+` AS t
		SET delivered_revision = GREATEST(t.delivered_revision, a.revision)
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::bigint[])
			AS a(actor_id, content_kind, content_id, content_version_id, axis, revision)
		WHERE t.tenant_id = $1 AND t.actor_id = a.actor_id AND t.content_kind = a.content_kind
		  AND t.content_id = a.content_id AND t.content_version_id = a.content_version_id AND t.axis = a.axis
		  AND t.revision >= a.revision`,
		rt.tenant, actors, kinds, ids, versions, axes, revs)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != n {
		return fmt.Errorf("content: %d of %d preference acks name a revision no snapshot holds", n-int(tag.RowsAffected()), n)
	}
	return nil
}

// SeedPreferenceRevisionFloor raises the schema's revision sequence so every
// future revision exceeds floor, and returns the resulting floor. It only ever
// advances (a repeat run, or a floor below the current one, is a no-op) and
// fails closed outside the safe range. Required before the first export into
// a sink that holds timestamp-derived revisions: starting at 1 would make
// every new revision lose to the old domain forever.
func (rt *Runtime) SeedPreferenceRevisionFloor(ctx context.Context, floor int64) (int64, error) {
	if floor < 0 || floor > maxPreferenceRevisionFloor {
		return 0, fmt.Errorf("content: preference revision floor %d is outside the safe range [0, %d]", floor, maxPreferenceRevisionFloor)
	}
	var current, used int64
	var called bool
	if err := rt.store.pool.QueryRow(ctx, `SELECT last_value, is_called FROM `+rt.store.revisionSeq).Scan(&current, &called); err != nil {
		return 0, err
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM `+rt.store.t.preferenceSnapshots).Scan(&used); err != nil {
		return 0, err
	}
	want := max(max(floor, current), used)
	if want == current && (called || floor < current && used < current) {
		return current, nil
	}
	if _, err := rt.store.pool.Exec(ctx, `SELECT setval('`+rt.store.revisionSeq+`', $1)`, want); err != nil {
		return 0, err
	}
	return want, nil
}

// PurgePreferenceSubjects removes the tenant's snapshots and cutover archive of erased actors: the
// terminal deletion fence, not a delivery outcome. Returns snapshot rows removed.
func (rt *Runtime) PurgePreferenceSubjects(ctx context.Context, actorIDs []string) (int64, error) {
	if len(actorIDs) == 0 {
		return 0, nil
	}
	tx, err := rt.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.preferenceSnapshots+` WHERE tenant_id = $1 AND actor_id = ANY($2)`, rt.tenant, actorIDs)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.preferenceArchive+` WHERE tenant_id = $1 AND actor_id = ANY($2)`, rt.tenant, actorIDs); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// --- cutover: collapse per-language source rows onto canonical references ---

// PreferenceMigrationOptions configures MigratePreferences.
type PreferenceMigrationOptions struct {
	// ExportedKeys are keys a previous exporter already sent. Any with no
	// surviving source row is seeded as a zero snapshot, so a sink cannot keep
	// counting a preference this host no longer holds. OccurredAt for such a
	// row is the cutover time: an explicit reconciliation, not invented history.
	ExportedKeys []PreferenceKey
	// DryRun computes and reports the migration, then rolls it back.
	DryRun bool
}

// PreferenceConflict is one (actor, reference, axis) group whose source rows
// disagreed or were several, resolved by the documented rule.
type PreferenceConflict struct {
	PreferenceKey
	SourceIDs    []string // the pre-collapse content ids
	SourceValues []int16
	Resolved     int16
}

// PreferenceMigration reports what MigratePreferences did.
type PreferenceMigration struct {
	ArchivedRows    int64
	ReactionRows    int64 // reaction rows after collapse
	FavoriteRows    int64 // favorite rows after collapse
	Conflicts       []PreferenceConflict
	Seeded          int64 // snapshots written from source truth
	Tombstoned      int64 // zero snapshots for previously exported, now-absent keys
	CountsRewritten int64
	DryRun          bool
}

// MigratePreferences is the one-time cutover to canonical preference identity:
// it archives every preference-bearing reaction/favorite row of the tenant,
// collapses each (actor, canonical reference, axis) group (a dislike wins,
// else a like, else neutral; a favorite survives if any row had it),
// recomputes the affected rollup counts from the surviving rows, and seeds a
// snapshot per surviving row plus a zero snapshot for every previously
// exported key that no longer has one. Comment reactions and declined targets
// are untouched.
//
// It runs in one transaction with the writers paused, is idempotent, never
// overwrites a newer live preference, and reports every conflicting group.
func (rt *Runtime) MigratePreferences(ctx context.Context, opts PreferenceMigrationOptions) (PreferenceMigration, error) {
	report := PreferenceMigration{DryRun: opts.DryRun}
	if rt.preferences.canon == nil {
		return report, errors.New("content: MigratePreferences needs Options.Canonicalizer")
	}
	tx, err := rt.store.pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	// Cutover/restore runs with ALL source writers (including erasure) paused.
	// Reject restored source rows for fenced accounts before archiving or reseeding;
	// the host must replay accepted erasures before resuming cutover/traffic.
	var erasedSource bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM `+rt.privateFences()+` f WHERE f.tenant_id=$1 AND (
 EXISTS (SELECT 1 FROM `+rt.store.t.reactions+` r WHERE r.tenant_id=f.tenant_id AND r.user_id=f.actor_id)
 OR EXISTS (SELECT 1 FROM `+rt.store.t.favorites+` v WHERE v.tenant_id=f.tenant_id AND v.user_id=f.actor_id)))`, rt.tenant).Scan(&erasedSource); err != nil {
		return report, err
	}
	if erasedSource {
		return report, ErrSubjectErased
	}
	for _, axis := range []string{PreferenceAxisReaction, PreferenceAxisFavorite} {
		if err := rt.preferences.migrateAxis(ctx, tx, &report, axis); err != nil {
			return report, err
		}
	}
	if err := rt.preferences.recomputeCounts(ctx, tx, &report); err != nil {
		return report, err
	}
	if err := rt.preferences.tombstoneExported(ctx, tx, &report, opts.ExportedKeys); err != nil {
		return report, err
	}
	if opts.DryRun {
		return report, nil
	}
	return report, tx.Commit(ctx)
}

// sourceRow is one pre-collapse reaction/favorite row.
type sourceRow struct {
	actor, ip string
	ref       contentref.ContentRef
	value     int16
	at        time.Time
}

// migrateAxis archives, collapses and seeds one axis. Groups are processed in
// key order so a concurrent bulk operation would take keys in a fixed order.
func (p *preferences) migrateAxis(ctx context.Context, tx pgx.Tx, report *PreferenceMigration, axis string) error {
	var query, table string
	switch axis {
	case PreferenceAxisReaction:
		table = p.s.t.reactions
		query = `SELECT COALESCE(user_id, ''), COALESCE(ip, ''), content_kind, content_id, content_version_id, value, updated_at
			FROM ` + table + ` WHERE tenant_id = $1 AND content_kind <> '` + KindComment + `'
			ORDER BY content_kind, content_id, content_version_id, user_id, ip`
	default:
		table = p.s.t.favorites
		query = `SELECT user_id, '', content_kind, content_id, content_version_id, 1::smallint, created_at
			FROM ` + table + ` WHERE tenant_id = $1 ORDER BY content_kind, content_id, content_version_id, user_id`
	}
	rows, err := tx.Query(ctx, query, p.s.tenant)
	if err != nil {
		return err
	}
	// groupKey keeps anonymous (ip) rows separate from actor rows: they share
	// the canonical reference but never the snapshot.
	type groupKey struct {
		actor, ip string
		ref       contentref.ContentKey
	}
	groups := make(map[groupKey][]sourceRow)
	var order []groupKey
	for rows.Next() {
		var r sourceRow
		var kind, id, version string
		if err := rows.Scan(&r.actor, &r.ip, &kind, &id, &version, &r.value, &r.at); err != nil {
			rows.Close()
			return err
		}
		r.ref = contentref.NewVersion(p.s.tenant, kind, id, version)
		w, ok := p.work(r.ref)
		if !ok {
			continue
		}
		k := groupKey{actor: r.actor, ref: w.Key()}
		if r.actor == "" {
			k.ip = r.ip
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	b := newBatcher(ctx, tx, 500)
	for _, k := range order {
		for _, r := range groups[k] {
			report.ArchivedRows++
			if err := b.queue(`INSERT INTO `+p.s.t.preferenceArchive+`
				(tenant_id, axis, actor_id, ip, content_kind, content_id, content_version_id, value, source_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				p.s.tenant, axis, nullIf(r.actor), nullIf(r.ip), r.ref.ContentKind, r.ref.ContentID, r.ref.Version(), r.value, r.at); err != nil {
				return err
			}
		}
	}
	if err := b.flush(); err != nil {
		return err
	}

	// Wipe the preference-bearing rows and rewrite the collapsed ones. Declined
	// targets and comment reactions never entered a group and keep their rows.
	for _, k := range order {
		group := groups[k]
		value, at := collapse(axis, group)
		conflict := PreferenceConflict{
			PreferenceKey: PreferenceKey{TenantID: k.ref.TenantID, ActorID: k.actor, ContentKind: k.ref.ContentKind, ContentID: k.ref.ContentID, ContentVersionID: k.ref.ContentVersionID, Axis: axis},
			Resolved:      value,
		}
		conflicting := len(group) > 1
		for _, r := range group {
			conflict.SourceIDs = append(conflict.SourceIDs, r.ref.ContentID)
			conflict.SourceValues = append(conflict.SourceValues, r.value)
			if r.value != value {
				conflicting = true
			}
		}
		if conflicting {
			report.Conflicts = append(report.Conflicts, conflict)
		}
		for _, r := range group {
			if axis == PreferenceAxisReaction {
				if err := b.queue(`DELETE FROM `+table+` WHERE `+keyPred(1)+` AND user_id IS NOT DISTINCT FROM $5 AND ip IS NOT DISTINCT FROM $6`,
					append(keyArgs(r.ref.Key()), nullIf(r.actor), nullIf(r.ip))...); err != nil {
					return err
				}
				continue
			}
			if err := b.queue(`DELETE FROM `+table+` WHERE `+keyPred(1)+` AND user_id = $5`, append(keyArgs(r.ref.Key()), r.actor)...); err != nil {
				return err
			}
		}
		if axis == PreferenceAxisReaction {
			if err := b.queue(`INSERT INTO `+table+` (`+keyCols+`, user_id, ip, value, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
				append(keyArgs(k.ref), nullIf(k.actor), nullIf(k.ip), value, at)...); err != nil {
				return err
			}
			report.ReactionRows++
		} else {
			if err := b.queue(`INSERT INTO `+table+` (`+keyCols+`, user_id, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				append(keyArgs(k.ref), k.actor, at)...); err != nil {
				return err
			}
			report.FavoriteRows++
		}
		if k.actor == "" { // anonymous rows carry no analytics subject
			continue
		}
		// A snapshot older than the one already held is history losing to a live
		// mutation; the WHERE keeps the live row.
		if err := b.queue(`INSERT INTO `+p.s.t.preferenceSnapshots+` AS t
			(tenant_id, actor_id, content_kind, content_id, content_version_id, axis, value, revision, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, nextval('`+p.s.revisionSeq+`'), $8)
			ON CONFLICT (tenant_id, actor_id, content_kind, content_id, content_version_id, axis) DO UPDATE
			SET value = EXCLUDED.value, revision = EXCLUDED.revision, occurred_at = EXCLUDED.occurred_at
			WHERE t.occurred_at < EXCLUDED.occurred_at`,
			append(conflict.PreferenceKey.args(), value, at)...); err != nil {
			return err
		}
		report.Seeded++
	}
	return b.flush()
}

// batcher pipelines statements inside one transaction, flushing every size
// queued statements. Errors surface at flush, which fails the whole migration.
type batcher struct {
	ctx   context.Context
	tx    pgx.Tx
	size  int
	batch *pgx.Batch
}

func newBatcher(ctx context.Context, tx pgx.Tx, size int) *batcher {
	return &batcher{ctx: ctx, tx: tx, size: size, batch: &pgx.Batch{}}
}

func (b *batcher) queue(sql string, args ...any) error {
	b.batch.Queue(sql, args...)
	if b.batch.Len() < b.size {
		return nil
	}
	return b.flush()
}

func (b *batcher) flush() error {
	if b.batch.Len() == 0 {
		return nil
	}
	results := b.tx.SendBatch(b.ctx, b.batch)
	b.batch = &pgx.Batch{}
	if _, err := results.Exec(); err != nil {
		results.Close()
		return err
	}
	return results.Close()
}

// collapse resolves a group: a dislike wins over a like, a like over neutral;
// a favorite survives if any row had it. The retained time is the group's
// newest source time for reactions (the latest real act) and its oldest for
// favorites (the bookmark's age, which orders the wishlist).
func collapse(axis string, group []sourceRow) (int16, time.Time) {
	if axis == PreferenceAxisFavorite {
		at := group[0].at
		for _, r := range group[1:] {
			if r.at.Before(at) {
				at = r.at
			}
		}
		return 1, at
	}
	var value int16
	at := group[0].at
	for _, r := range group {
		if r.value == -1 || (r.value == 1 && value == 0) {
			value = r.value
		}
		if r.at.After(at) {
			at = r.at
		}
	}
	return value, at
}

// recomputeCounts rewrites likes/dislikes/favorites on every rollup row of the
// tenant the collapse could have moved, from the surviving source rows.
// comment_count is untouched: comment threads keep their own reference.
func (p *preferences) recomputeCounts(ctx context.Context, tx pgx.Tx, report *PreferenceMigration) error {
	tag, err := tx.Exec(ctx, `WITH touched AS (
			SELECT content_kind, content_id, content_version_id FROM `+p.s.t.preferenceArchive+` WHERE tenant_id = $1
			UNION SELECT content_kind, content_id, content_version_id FROM `+p.s.t.reactions+` WHERE tenant_id = $1 AND content_kind <> '`+KindComment+`'
			UNION SELECT content_kind, content_id, content_version_id FROM `+p.s.t.favorites+` WHERE tenant_id = $1
			UNION SELECT content_kind, content_id, content_version_id FROM `+p.s.t.counts+` WHERE tenant_id = $1
		), fresh AS (
			SELECT t.content_kind, t.content_id, t.content_version_id,
				(SELECT count(*) FROM `+p.s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = t.content_kind AND r.content_id = t.content_id AND r.content_version_id = t.content_version_id AND r.value = 1) AS likes,
				(SELECT count(*) FROM `+p.s.t.reactions+` r WHERE r.tenant_id = $1 AND r.content_kind = t.content_kind AND r.content_id = t.content_id AND r.content_version_id = t.content_version_id AND r.value = -1) AS dislikes,
				(SELECT count(*) FROM `+p.s.t.favorites+` f WHERE f.tenant_id = $1 AND f.content_kind = t.content_kind AND f.content_id = t.content_id AND f.content_version_id = t.content_version_id) AS favorites
			FROM touched t
		)
		INSERT INTO `+p.s.t.counts+` (`+keyCols+`, likes, dislikes, favorites, comment_count)
		SELECT $1, content_kind, content_id, content_version_id, likes, dislikes, favorites, 0 FROM fresh
		ON CONFLICT (`+keyCols+`) DO UPDATE
		SET likes = EXCLUDED.likes, dislikes = EXCLUDED.dislikes, favorites = EXCLUDED.favorites, updated_at = now()`, p.s.tenant)
	if err != nil {
		return err
	}
	report.CountsRewritten = tag.RowsAffected()
	return nil
}

// MaxExportedPreferencesPerBatch bounds one cutover key-reconciliation call.
const MaxExportedPreferencesPerBatch = 1000

// ReconcileExportedPreferences reconciles one bounded page of previously
// exported keys after MigratePreferences has seeded source truth. Missing keys
// receive zero snapshots; current snapshots keep their exact value/revision/time.
// Canonicalization uses the same host rule as live preferences. Fenced subjects
// are skipped. The inserted count excludes existing snapshots and duplicates.
//
// This is an explicit cutover operation: pause all source/delivery writers and
// retire old callbacks, seed the revision floor above retained sink/source state,
// then seed source truth before calling it. Persist the exported-key inventory
// before retiring old sink identities. Retrying a page is idempotent.
func (rt *Runtime) ReconcileExportedPreferences(ctx context.Context, keys []PreferenceKey) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	if len(keys) > MaxExportedPreferencesPerBatch {
		return 0, fmt.Errorf("content: exported preference batch exceeds %d", MaxExportedPreferencesPerBatch)
	}
	if rt.preferences.canon == nil {
		return 0, errors.New("content: exported preference reconciliation requires a Canonicalizer")
	}
	unique := map[PreferenceKey]struct{}{}
	actors := map[string]struct{}{}
	for _, k := range keys {
		if k.TenantID == "" {
			k.TenantID = rt.tenant
		}
		if k.TenantID != rt.tenant {
			return 0, ErrTenant
		}
		if strings.TrimSpace(k.ActorID) == "" || k.ContentID == "" || k.ContentKind == "" {
			return 0, badRequest("exported preference key is incomplete")
		}
		if k.Axis != PreferenceAxisReaction && k.Axis != PreferenceAxisFavorite {
			return 0, badRequest("unknown preference axis")
		}
		if err := rt.checkRef(k.Ref()); err != nil {
			return 0, err
		}
		ref, ok := rt.preferences.work(k.Ref())
		if !ok {
			return 0, badRequest("exported preference kind is not canonicalized")
		}
		k.ContentKind, k.ContentID, k.ContentVersionID = ref.ContentKind, ref.ContentID, ref.Version()
		unique[k] = struct{}{}
		actors[k.ActorID] = struct{}{}
	}
	ids := make([]string, 0, len(actors))
	for id := range actors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tx, err := rt.store.beginMutation(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		if err := rt.lockPrivateSubject(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	normalized := make([]PreferenceKey, 0, len(unique))
	for k := range unique {
		normalized = append(normalized, k)
	}
	sort.Slice(normalized, func(i, j int) bool {
		a, b := normalized[i], normalized[j]
		if a.ActorID != b.ActorID {
			return a.ActorID < b.ActorID
		}
		if a.ContentKind != b.ContentKind {
			return a.ContentKind < b.ContentKind
		}
		if a.ContentID != b.ContentID {
			return a.ContentID < b.ContentID
		}
		if a.ContentVersionID != b.ContentVersionID {
			return a.ContentVersionID < b.ContentVersionID
		}
		return a.Axis < b.Axis
	})
	var report PreferenceMigration
	if err := rt.preferences.tombstoneExported(ctx, tx, &report, normalized); err != nil {
		return 0, err
	}
	return report.Tombstoned, tx.Commit(ctx)
}

// tombstoneExported seeds a zero snapshot for every previously exported key
// with no surviving source row.
func (p *preferences) tombstoneExported(ctx context.Context, tx pgx.Tx, report *PreferenceMigration, keys []PreferenceKey) error {
	for _, k := range keys {
		if k.TenantID == "" {
			k.TenantID = p.s.tenant
		}
		if k.TenantID != p.s.tenant {
			return ErrTenant
		}
		if k.ActorID == "" || k.ContentKind == "" || k.ContentID == "" {
			return fmt.Errorf("content: exported key %+v is incomplete", k)
		}
		if k.Axis != PreferenceAxisReaction && k.Axis != PreferenceAxisFavorite {
			return fmt.Errorf("content: exported key %+v has no known axis", k)
		}
		if err := p.rt.privateSubjectAllowed(ctx, tx, k.ActorID); err != nil {
			if errors.Is(err, ErrSubjectErased) {
				continue
			}
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.preferenceSnapshots+`
			(tenant_id, actor_id, content_kind, content_id, content_version_id, axis, value, revision, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, 0, nextval('`+p.s.revisionSeq+`'), clock_timestamp())
			ON CONFLICT (tenant_id, actor_id, content_kind, content_id, content_version_id, axis) DO NOTHING`, k.args()...)
		if err != nil {
			return err
		}
		report.Tombstoned += tag.RowsAffected()
	}
	return nil
}

// --- bounded delivery ------------------------------------------------------

// PreferenceDisposition is a sink's verdict for one delivered snapshot. A void
// callback is not proof of delivery: only PreferenceAccepted acknowledges, and
// only after the sink durably accepted the row.
type PreferenceDisposition int

const (
	// PreferenceRetry is a transient failure: the row stays pending.
	PreferenceRetry PreferenceDisposition = iota
	// PreferenceAccepted means the sink durably holds this revision.
	PreferenceAccepted
	// PreferenceSubjectErased is terminal: the subject is erased at the sink, so
	// the obligation is purged instead of retried.
	PreferenceSubjectErased
)

// PreferenceSink receives immutable copies of snapshots and reports one
// disposition per snapshot, in order. An error fails the whole page (every row
// stays pending). Delivery is at-least-once: a crash after the sink accepted
// and before the acknowledgement replays the same revision.
type PreferenceSink interface {
	DeliverPreferences(ctx context.Context, snaps []PreferenceSnapshot) ([]PreferenceDisposition, error)
}

// PreferenceDelivery reports one sweep.
type PreferenceDelivery struct {
	Delivered    int
	Acknowledged int
	Retried      int
	Erased       int
	// Next is the key the sweep stopped at (zero when it ran out); a replay
	// interrupted by maxRows resumes from it.
	Next PreferenceKey
}

// DeliverPreferences continues one bounded sweep from after: it pages the tenant's pending
// snapshots in key order, hands each page to the sink, acknowledges exactly the
// revisions the sink accepted, purges erased subjects and leaves the rest
// pending. pageSize bounds each page; maxRows bounds the sweep (<= 0: until
// the cursor runs out). The cursor is per-sweep, so a key that keeps failing
// never starves the rest. Resume from Next even after a sink error. Once Next
// is zero the sweep is exhausted; start the next sweep from zero. Keep this
// cursor only for the current sweep, never as a persistent high-water mark.
func (rt *Runtime) DeliverPreferences(ctx context.Context, sink PreferenceSink, after PreferenceKey, pageSize, maxRows int) (PreferenceDelivery, error) {
	return rt.sweep(ctx, sink, after, pageSize, maxRows, rt.PendingPreferences)
}

// ReplayPreferences is the bounded full-snapshot replay that repairs sink
// loss: every snapshot from after, acknowledged rows and zeros included, is
// delivered with its persisted revision, time and value, and acknowledged on
// acceptance. Concurrent newer snapshots still win at the sink by revision.
// Resume an interrupted replay from the returned Next.
func (rt *Runtime) ReplayPreferences(ctx context.Context, sink PreferenceSink, after PreferenceKey, pageSize, maxRows int) (PreferenceDelivery, error) {
	return rt.sweep(ctx, sink, after, pageSize, maxRows, rt.ScanPreferences)
}

func (rt *Runtime) sweep(ctx context.Context, sink PreferenceSink, after PreferenceKey, pageSize, maxRows int, page func(context.Context, PreferenceKey, int) ([]PreferenceSnapshot, error)) (PreferenceDelivery, error) {
	out := PreferenceDelivery{Next: after}
	if sink == nil {
		return out, errors.New("content: DeliverPreferences needs a sink")
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	for maxRows <= 0 || out.Delivered < maxRows {
		limit := pageSize
		if maxRows > 0 && maxRows-out.Delivered < limit {
			limit = maxRows - out.Delivered
		}
		snaps, err := page(ctx, after, limit)
		if err != nil {
			return out, err
		}
		if len(snaps) == 0 {
			out.Next = PreferenceKey{}
			return out, nil
		}
		after = snaps[len(snaps)-1].PreferenceKey
		out.Next = after
		dispositions, err := sink.DeliverPreferences(ctx, append([]PreferenceSnapshot(nil), snaps...))
		if err != nil {
			out.Retried += len(snaps)
			return out, err
		}
		if len(dispositions) != len(snaps) {
			return out, fmt.Errorf("content: sink returned %d dispositions for %d snapshots", len(dispositions), len(snaps))
		}
		out.Delivered += len(snaps)
		var acks []PreferenceAck
		var erased []string
		for i, d := range dispositions {
			switch d {
			case PreferenceAccepted:
				acks = append(acks, PreferenceAck{PreferenceKey: snaps[i].PreferenceKey, Revision: snaps[i].Revision})
			case PreferenceSubjectErased:
				erased = append(erased, snaps[i].ActorID)
			default:
				out.Retried++
			}
		}
		if err := rt.AcknowledgePreferences(ctx, acks); err != nil {
			return out, err
		}
		out.Acknowledged += len(acks)
		if len(erased) > 0 {
			n, err := rt.PurgePreferenceSubjects(ctx, erased)
			if err != nil {
				return out, err
			}
			out.Erased += int(n)
		}
		if len(snaps) < limit {
			out.Next = PreferenceKey{}
			return out, nil
		}
	}
	return out, nil
}

// revisionSeqName is the schema-local sequence's identifier.
func revisionSeqName(schema string) string {
	return pgx.Identifier{schema, "content_preference_revision_seq"}.Sanitize()
}
