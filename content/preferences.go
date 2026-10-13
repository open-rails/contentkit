package content

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
)

// The preference boundary: an authenticated actor's current reaction
// (-1/0/1) and favorite (1/0) per reference, read straight from
// content_reactions and content_favorites. Every value change stamps the row
// with a revision from the schema's sequence; neutral and unfavorite keep a
// zero row. SyncPreferences exports rows past a watermark, newest revision wins.

// Preference axes: the signal Type each table exports under.
const (
	PreferenceAxisReaction = "reaction" // value -1 dislike / 0 neutral / 1 like
	PreferenceAxisFavorite = "favorite" // value 1 favorited / 0 not
)

// DefaultPreferenceSyncOverlap is the default Options.PreferenceSyncOverlap.
const DefaultPreferenceSyncOverlap = 5 * time.Minute

// maxPreferenceRevisionFloor bounds a seeded revision floor well inside bigint.
const maxPreferenceRevisionFloor = math.MaxInt64 / 2

const preferencePageSize = 500

// preferences exports rows.
type preferences struct {
	rt      *Runtime
	s       *store
	overlap time.Duration
}

func newPreferences(rt *Runtime, overlap time.Duration) *preferences {
	if overlap <= 0 {
		overlap = DefaultPreferenceSyncOverlap
	}
	return &preferences{rt: rt, s: rt.store, overlap: overlap}
}

// work is the reference a target's preference is stored and exported under:
// the resolver's canonical one. Comment reactions are not preferences.
func (p *preferences) work(ref contentref.ContentRef) (contentref.ContentRef, bool) {
	return ref, ref.ContentKind != KindComment
}

// Preference is one exported row: an actor's current value on one axis of a
// canonical reference, with the revision and time of its last change.
type Preference struct {
	contentref.ContentRef
	ActorID   string
	Axis      string
	Value     int16
	Revision  int64
	UpdatedAt time.Time
}

// PreferenceSender durably writes one page of preferences; an error aborts the
// sync without advancing the watermark. Re-sends must be idempotent.
type PreferenceSender func(ctx context.Context, page []Preference) error

// PreferenceSyncReport reports one sync: the revision it scanned after and the
// rows it sent.
type PreferenceSyncReport struct {
	From int64
	Sent int
}

// SyncPreferences sends every exportable row whose revision is past the
// watermark, in revision order, then records a checkpoint. No connection is
// held across send. Revisions are allocated before commit, so each scan
// restarts from the newest checkpoint taken at least Options.PreferenceSyncOverlap
// before the latest one: a row is delivered as long as its transaction commits
// within the overlap of allocating its revision (ResyncPreferences covers the
// rest). A checkpoint is recorded only after its own scan was sent, so
// overlapping syncs stay correct and only repeat sends. send is required.
func (rt *Runtime) SyncPreferences(ctx context.Context, send PreferenceSender) (PreferenceSyncReport, error) {
	p := rt.preferences
	if send == nil {
		return PreferenceSyncReport{}, fmt.Errorf("content: preference sender is required")
	}
	pool := rt.store.pool
	var mark int64
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END, clock_timestamp() FROM `+rt.store.revisionSeq).Scan(&mark, &at); err != nil {
		return PreferenceSyncReport{}, err
	}
	var from int64
	var fromAt time.Time
	err := pool.QueryRow(ctx, `SELECT revision, taken_at FROM `+rt.store.t.preferenceSync+`
		WHERE tenant_id = $1 AND taken_at <= (SELECT max(taken_at) FROM `+rt.store.t.preferenceSync+` WHERE tenant_id = $1) - make_interval(secs => $2)
		ORDER BY taken_at DESC LIMIT 1`, rt.tenant, p.overlap.Seconds()).Scan(&from, &fromAt)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PreferenceSyncReport{}, err
	}
	report, err := p.export(ctx, pool, from, send)
	if err != nil {
		return report, err
	}
	return report, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO `+rt.store.t.preferenceSync+` (tenant_id, taken_at, revision) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, rt.tenant, at, mark); err != nil {
			return err
		}
		if found {
			_, err = tx.Exec(ctx, `DELETE FROM `+rt.store.t.preferenceSync+` WHERE tenant_id = $1 AND taken_at < $2`, rt.tenant, fromAt)
		}
		return err
	})
}

// ResyncPreferences re-sends every exportable row (zeros included): the
// periodic safety net for sink loss and slower-than-overlap commits.
// A sender is required.
func (rt *Runtime) ResyncPreferences(ctx context.Context, send PreferenceSender) (PreferenceSyncReport, error) {
	if send == nil {
		return PreferenceSyncReport{}, fmt.Errorf("content: preference sender is required")
	}
	return rt.preferences.export(ctx, rt.store.pool, 0, send)
}

// export pages rows with revision > from in revision order through send.
// Anonymous rows and comment threads are skipped.
func (p *preferences) export(ctx context.Context, q querier, from int64, send PreferenceSender) (PreferenceSyncReport, error) {
	report := PreferenceSyncReport{From: from}
	for after := from; ; {
		page, last, n, err := p.page(ctx, q, after)
		if err != nil {
			return report, err
		}
		if len(page) > 0 {
			if err := send(ctx, page); err != nil {
				return report, err
			}
			report.Sent += len(page)
		}
		if n < preferencePageSize {
			return report, nil
		}
		after = last
	}
}

func (p *preferences) page(ctx context.Context, q querier, after int64) (page []Preference, last int64, n int, err error) {
	branch := func(axis, table string) string {
		return `(SELECT '` + axis + `' AS axis, user_id, content_kind, content_id, content_version_id, value, revision, updated_at FROM ` + table + ` x
			WHERE tenant_id = $1 AND revision > $2 AND user_id IS NOT NULL AND content_kind <> '` + KindComment + `'
			  AND NOT EXISTS (SELECT 1 FROM ` + p.rt.erasedSubjectsTable() + ` f WHERE f.tenant_id = $1 AND f.actor_id = x.user_id)
			ORDER BY revision LIMIT $3)`
	}
	rows, err := q.Query(ctx, `SELECT * FROM (`+branch(PreferenceAxisReaction, p.s.t.reactions)+` UNION ALL `+branch(PreferenceAxisFavorite, p.s.t.favorites)+`) u
		ORDER BY revision LIMIT $3`, p.s.tenant, after, preferencePageSize)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Preference
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
