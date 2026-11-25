package contentstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
)

// maxPreferenceRevisionFloor bounds a seeded revision floor well inside bigint.
const maxPreferenceRevisionFloor = math.MaxInt64 / 2

const (
	// preferenceBatch is the rows of one export batch, sent as one insert.
	preferenceBatch = 1000
	// preferenceBudget bounds one sync run; a run also keeps half of the
	// time to its context's deadline in reserve.
	preferenceBudget = 30 * time.Second
)

// preferences exports rows.
type preferences struct {
	rt      *Runtime
	s       *store
	overlap time.Duration
	now     func() time.Time
}

func newPreferences(rt *Runtime, overlap time.Duration, now func() time.Time) *preferences {
	if overlap <= 0 {
		overlap = content.DefaultPreferenceSyncOverlap
	}
	if now == nil {
		now = time.Now
	}
	return &preferences{rt: rt, s: rt.store, overlap: overlap, now: now}
}

// work is the reference a target's preference is stored and exported under:
// the resolver's canonical one. Comment reactions are not preferences.
func (p *preferences) work(ref contentref.ContentRef) (contentref.ContentRef, bool) {
	return ref, ref.ContentKind != content.KindComment
}

// SyncPreferences sends each exportable row once, in revision order: those
// past the tenant's cursor and settled, at or below the newest sample of the
// revision sequence taken at least Options.PreferenceSyncOverlap ago.
// Revisions are allocated before commit, so a row is delivered as long as its
// transaction commits within the overlap of allocating its revision. Rows go
// in bounded batches, one send each, and the cursor advances after every
// batch, so a run cut short resumes where it stopped and re-sends at most the
// batch in flight. A run starts no batch past its budget or too close to its
// deadline, and then reports More. A cursor another run (or
// ResyncPreferences) moved stops this one. No connection is held across send.
// send is required.
func (rt *Runtime) SyncPreferences(ctx context.Context, send content.PreferenceSender) (content.PreferenceSyncReport, error) {
	p := rt.preferences
	if send == nil {
		return content.PreferenceSyncReport{}, fmt.Errorf("content: preference sender is required")
	}
	settled, err := p.sample(ctx)
	if err != nil {
		return content.PreferenceSyncReport{}, err
	}
	var cursor int64
	err = rt.store.pool.QueryRow(ctx, `SELECT revision FROM `+rt.store.t.preferenceCursor+` WHERE tenant_id = $1`, rt.tenant).Scan(&cursor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return content.PreferenceSyncReport{}, err
	}
	report := content.PreferenceSyncReport{From: cursor}
	budget, deadline := preferenceBudget, time.Time{}
	if d, ok := ctx.Deadline(); ok {
		budget, deadline = min(budget, time.Until(d)/2), d
	}
	started := time.Now()
	var slowest time.Duration
	for cursor < settled {
		if time.Since(started) >= budget || !deadline.IsZero() && time.Until(deadline) < 2*slowest {
			report.More = true
			return report, nil
		}
		batchStarted := time.Now()
		page, last, count, err := p.page(ctx, rt.store.pool, cursor, settled, preferenceBatch)
		if err != nil {
			return report, err
		}
		if len(page) > 0 {
			if err := send(ctx, page); err != nil {
				return report, err
			}
			report.Sent += len(page)
		}
		if count < preferenceBatch {
			last = settled // nothing else up to settled exports
		}
		if moved, err := p.advance(ctx, cursor, last); err != nil || !moved {
			return report, err
		}
		cursor = last
		slowest = max(slowest, time.Since(batchStarted))
	}
	return report, nil
}

// sample records the revision sequence's value now and returns the newest
// sample at least the overlap old (0 when there is none yet): every revision
// up to it was allocated at least the overlap ago. Older samples go.
func (p *preferences) sample(ctx context.Context) (int64, error) {
	s, now := p.s, p.now().UTC()
	if _, err := s.pool.Exec(ctx, `INSERT INTO `+s.t.preferenceSync+` (tenant_id, taken_at, revision)
		SELECT $1, $2, CASE WHEN is_called THEN last_value ELSE 0 END FROM `+s.revisionSeq+` ON CONFLICT DO NOTHING`, s.tenant, now); err != nil {
		return 0, err
	}
	var settled int64
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT revision, taken_at FROM `+s.t.preferenceSync+`
		WHERE tenant_id = $1 AND taken_at <= $2 ORDER BY taken_at DESC LIMIT 1`, s.tenant, now.Add(-p.overlap)).Scan(&settled, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM `+s.t.preferenceSync+` WHERE tenant_id = $1 AND taken_at < $2`, s.tenant, at)
	return settled, err
}

// advance moves the tenant's cursor from one revision to the next; false when
// another run or a resync moved it first.
func (p *preferences) advance(ctx context.Context, from, to int64) (bool, error) {
	tag, err := p.s.pool.Exec(ctx, `INSERT INTO `+p.s.t.preferenceCursor+` AS c (tenant_id, revision) VALUES ($1, $3)
		ON CONFLICT (tenant_id) DO UPDATE SET revision = EXCLUDED.revision WHERE c.revision = $2`, p.s.tenant, from, to)
	return tag.RowsAffected() == 1, err
}

// ResyncPreferences rewinds the tenant's cursor, so the following syncs send
// every exportable row (zeros included) once more, in the same batches: the
// repair after the signal plane lost data.
func (rt *Runtime) ResyncPreferences(ctx context.Context) error {
	_, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.preferenceCursor+` SET revision = 0 WHERE tenant_id = $1`, rt.tenant)
	return err
}

// page reads up to limit exportable rows with after < revision <= upTo, in
// revision order. Anonymous rows, comment threads and erased accounts are
// skipped.
func (p *preferences) page(ctx context.Context, q querier, after, upTo int64, limit int) (page []content.Preference, last int64, n int, err error) {
	// Favorites are keyed by app_id and uuids; reactions by tenant_id and text.
	branch := func(axis, table, app, text string) string {
		return `(SELECT '` + axis + `' AS axis, user_id` + text + ` AS user_id, content_kind, content_id` + text + ` AS content_id,
			content_version_id` + text + ` AS content_version_id, value, revision, updated_at FROM ` + table + ` x
			WHERE ` + app + ` AND revision > $2 AND revision <= $4 AND user_id IS NOT NULL AND content_kind <> '` + content.KindComment + `'
			  AND NOT EXISTS (SELECT 1 FROM ` + p.rt.erasedSubjectsTable() + ` f WHERE f.tenant_id = $1 AND f.actor_id = x.user_id` + text + `)
			ORDER BY revision LIMIT $3)`
	}
	rows, err := q.Query(ctx, `SELECT * FROM (`+branch(content.PreferenceAxisReaction, p.s.t.reactions, "tenant_id = $1", "")+` UNION ALL `+
		branch(content.PreferenceAxisFavorite, p.s.t.favorites, "app_id = $1::text::uuid", "::text")+`) u
		ORDER BY revision LIMIT $3`, p.s.tenant, after, limit, upTo)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var r content.Preference
		var kind, id, version string
		if err := rows.Scan(&r.Axis, &r.ActorID, &kind, &id, versionid.Into(&version), &r.Value, &r.Revision, &r.UpdatedAt); err != nil {
			return nil, 0, 0, err
		}
		n++
		last = r.Revision
		r.ContentRef = contentref.NewVersion(p.s.tenant, kind, id, version)
		if _, ok := p.work(r.ContentRef); ok {
			page = append(page, r)
		}
	}
	return page, last, n, rows.Err()
}

// nextRevision is the SQL allocating a preference revision.
func (s *store) nextRevision() string { return `nextval('` + s.revisionSeq + `')` }

// revisionSeqName is the schema-local sequence's identifier.
func revisionSeqName(schema string) string {
	return pgx.Identifier{schema, "content_preference_revision_seq"}.Sanitize()
}
