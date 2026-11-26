// Package readingstore is the per-person reading state, history and identity
// links in PostgreSQL that the analytics job writes from ClickHouse's
// activity log, and the immediate writes beside it: hide, sign-in links,
// clearing history and erasure.
package readingstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is one app's reading state.
type Store struct {
	pool   *pgxpool.Pool
	app    uuid.UUID
	tenant string // the app id as the catalog's text tenant_id
	now    func() time.Time
	t      tables
}

type tables struct {
	state, history, links, active, job, erased, reactions, searches, items, memberships, favorites, feedback string
}

// New is app's store in schema; now is the engine's clock.
func New(pool *pgxpool.Pool, schema string, app uuid.UUID, now func() time.Time) *Store {
	q := func(t string) string { return pgx.Identifier{schema, t}.Sanitize() }
	return &Store{pool: pool, app: app, tenant: app.String(), now: now, t: tables{state: q("reading_state"), history: q("reading_history"),
		links: q("anonymous_links"), active: q("anonymous_activity"), job: q("analytics_job"), erased: q("content_erased_subjects"),
		reactions: q("content_reactions"), searches: q("content_searches"), items: q("content_items"), memberships: q("content_memberships"),
		favorites: q("content_favorites"), feedback: q("content_reaction_feedback")}}
}

// Outcome ordinals of reading_state.best_outcome.
const (
	OutcomeNone int16 = iota
	OutcomeBounce
	OutcomeNeutral
	OutcomeGood
	OutcomeComplete
)

// StateUpdate is one person × gallery × day merged into reading_state.
type StateUpdate struct {
	Subject, ContentID uuid.UUID
	Day, LastSeen      time.Time
	Outcome            int16
	Furthest           uint32
	// Position is the day's last reported point; nil when none was.
	Position *uint32
}

// notErased filters subjects of recorded erasures; $1 is the tenant.
func (s *Store) notErased(subject string) string {
	return `NOT EXISTS (SELECT 1 FROM ` + s.t.erased + ` e WHERE e.tenant_id = $1 AND e.actor_id = (` + subject + `)::text)`
}

// mergeSet is the max/latest merge every write of a read uses.
const mergeSet = `first_day = LEAST(r.first_day, EXCLUDED.first_day), last_seen_at = GREATEST(r.last_seen_at, EXCLUDED.last_seen_at),
 best_outcome = GREATEST(r.best_outcome, EXCLUDED.best_outcome), furthest = GREATEST(r.furthest, EXCLUDED.furthest),
 "position" = CASE WHEN EXCLUDED."position" IS NOT NULL AND (r."position" IS NULL OR r.last_seen_at IS NULL OR EXCLUDED.last_seen_at >= r.last_seen_at)
  THEN EXCLUDED."position" ELSE r."position" END`

// collapse merges updates of one person × gallery (several days) by
// mergeSet's rules, since one INSERT may not update a row twice.
func collapse(ups []StateUpdate) []StateUpdate {
	type key struct{ subject, content uuid.UUID }
	at := make(map[key]int, len(ups))
	out := make([]StateUpdate, 0, len(ups))
	for _, u := range ups {
		k := key{u.Subject, u.ContentID}
		i, ok := at[k]
		if !ok {
			at[k] = len(out)
			out = append(out, u)
			continue
		}
		a := &out[i]
		if u.Position != nil && (a.Position == nil || !u.LastSeen.Before(a.LastSeen)) {
			a.Position = u.Position
		}
		if u.Day.Before(a.Day) {
			a.Day = u.Day
		}
		if u.LastSeen.After(a.LastSeen) {
			a.LastSeen = u.LastSeen
		}
		a.Outcome, a.Furthest = max(a.Outcome, u.Outcome), max(a.Furthest, u.Furthest)
	}
	return out
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Merge merges the job's outcomes by maximum and latest: safe to repeat.
// Erased subjects are skipped.
func (s *Store) Merge(ctx context.Context, ups []StateUpdate) error {
	return s.MergeTx(ctx, s.pool, ups)
}

// MergeTx is Merge in q, a pool or a transaction.
func (s *Store) MergeTx(ctx context.Context, q execer, ups []StateUpdate) error {
	if len(ups) == 0 {
		return nil
	}
	ups = collapse(ups)
	n := len(ups)
	subjects, contents := make([]uuid.UUID, n), make([]uuid.UUID, n)
	days, seen := make([]time.Time, n), make([]time.Time, n)
	outcomes, furthest, positions := make([]int16, n), make([]int32, n), make([]*int32, n)
	for i, u := range ups {
		subjects[i], contents[i], days[i], seen[i], outcomes[i], furthest[i] = u.Subject, u.ContentID, utcDay(u.Day), u.LastSeen.UTC(), u.Outcome, clampInt32(u.Furthest)
		if u.Position != nil {
			p := clampInt32(*u.Position)
			positions[i] = &p
		}
	}
	_, err := q.Exec(ctx, `INSERT INTO `+s.t.state+` AS r (app_id, subject, content_id, first_day, last_seen_at, best_outcome, furthest, "position")
SELECT $2::uuid, u.subject, u.content_id, u.day, u.seen, u.outcome, u.furthest, u.position
FROM unnest($3::uuid[], $4::uuid[], $5::date[], $6::timestamptz[], $7::smallint[], $8::int[], $9::int[]) AS u(subject, content_id, day, seen, outcome, furthest, position)
WHERE `+s.notErased("u.subject")+`
ON CONFLICT (app_id, subject, content_id) DO UPDATE SET `+mergeSet,
		s.tenant, s.app, subjects, contents, days, seen, outcomes, furthest, positions)
	if err != nil {
		return fmt.Errorf("readingstore: merge reading state: %w", err)
	}
	return nil
}

// HistoryDay is one person's packed day.
type HistoryDay struct {
	Subject uuid.UUID
	Day     time.Time
	Actions []Action
}

// PutHistory rewrites people's days: each is recomputed whole, so repeating
// it is harmless. A day without actions is deleted.
func (s *Store) PutHistory(ctx context.Context, days []HistoryDay) error {
	if len(days) == 0 {
		return nil
	}
	var subjects, empty []uuid.UUID
	var dates, emptyDates []time.Time
	var packed [][]byte
	for _, d := range days {
		if len(d.Actions) == 0 {
			empty, emptyDates = append(empty, d.Subject), append(emptyDates, utcDay(d.Day))
			continue
		}
		subjects, dates, packed = append(subjects, d.Subject), append(dates, utcDay(d.Day)), append(packed, EncodeDay(d.Day, d.Actions))
	}
	b := &pgx.Batch{}
	if len(subjects) > 0 {
		b.Queue(`INSERT INTO `+s.t.history+` (app_id, subject, day, actions)
SELECT $2::uuid, u.subject, u.day, u.actions FROM unnest($3::uuid[], $4::date[], $5::bytea[]) AS u(subject, day, actions) WHERE `+s.notErased("u.subject")+`
ON CONFLICT (app_id, subject, day) DO UPDATE SET actions = EXCLUDED.actions`, s.tenant, s.app, subjects, dates, packed)
	}
	if len(empty) > 0 {
		b.Queue(`DELETE FROM `+s.t.history+` h USING unnest($2::uuid[], $3::date[]) AS u(subject, day)
WHERE h.app_id = $1 AND h.subject = u.subject AND h.day = u.day`, s.app, empty, emptyDates)
	}
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("readingstore: write reading history: %w", err)
	}
	return nil
}

// SetHidden hides or unhides a gallery for subject at once. Unhiding a
// gallery never opened removes its row.
func (s *Store) SetHidden(ctx context.Context, subject, content uuid.UUID, hidden bool) error {
	if hidden {
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.state+` AS r (app_id, subject, content_id, hidden)
SELECT $2::uuid, $3::uuid, $4::uuid, true WHERE `+s.notErased("$3::uuid")+`
ON CONFLICT (app_id, subject, content_id) DO UPDATE SET hidden = true`, s.tenant, s.app, subject, content); err != nil {
				return err
			}
			return s.Touch(ctx, tx, subject)
		})
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.state+` WHERE app_id = $1 AND subject = $2 AND content_id = $3 AND last_seen_at IS NULL`, s.app, subject, content); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE `+s.t.state+` SET hidden = false WHERE app_id = $1 AND subject = $2 AND content_id = $3`, s.app, subject, content)
		return err
	})
}

// Forget clears subjects' reading of contents (every gallery when contents
// is nil): their state and history. Hidden galleries stay hidden.
func (s *Store) Forget(ctx context.Context, subjects, contents []uuid.UUID) error {
	if len(subjects) == 0 {
		return nil
	}
	all := contents == nil
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		pick := ` AND (content_id = ANY($3) OR $4)`
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.state+` WHERE app_id = $1 AND subject = ANY($2) AND NOT hidden`+pick, s.app, subjects, nonNil(contents), all); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.state+` SET first_day = NULL, last_seen_at = NULL, best_outcome = 0, furthest = 0, "position" = NULL
WHERE app_id = $1 AND subject = ANY($2) AND hidden`+pick, s.app, subjects, nonNil(contents), all); err != nil {
			return err
		}
		if all {
			_, err := tx.Exec(ctx, `DELETE FROM `+s.t.history+` WHERE app_id = $1 AND subject = ANY($2)`, s.app, subjects)
			return err
		}
		return s.dropFromHistory(ctx, tx, subjects, contents)
	})
}

// dropFromHistory rewrites subjects' days without the galleries in drop.
func (s *Store) dropFromHistory(ctx context.Context, tx pgx.Tx, subjects, drop []uuid.UUID) error {
	gone := make(map[uuid.UUID]bool, len(drop))
	for _, id := range drop {
		gone[id] = true
	}
	rows, err := tx.Query(ctx, `SELECT subject, day, actions FROM `+s.t.history+` WHERE app_id = $1 AND subject = ANY($2) FOR UPDATE`, s.app, subjects)
	if err != nil {
		return err
	}
	type day struct {
		subject uuid.UUID
		day     time.Time
		actions []byte
	}
	days, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (day, error) {
		var d day
		err := r.Scan(&d.subject, &d.day, &d.actions)
		return d, err
	})
	if err != nil {
		return err
	}
	for _, d := range days {
		actions, err := DecodeDay(d.day, d.actions)
		if err != nil {
			return err
		}
		kept := actions[:0]
		for _, a := range actions {
			if a.Kind != ActionGallery || !gone[a.ID] {
				kept = append(kept, a)
			}
		}
		if len(kept) == len(actions) {
			continue
		}
		if len(kept) == 0 {
			_, err = tx.Exec(ctx, `DELETE FROM `+s.t.history+` WHERE app_id = $1 AND subject = $2 AND day = $3`, s.app, d.subject, d.day)
		} else {
			_, err = tx.Exec(ctx, `UPDATE `+s.t.history+` SET actions = $4 WHERE app_id = $1 AND subject = $2 AND day = $3`, s.app, d.subject, d.day, EncodeDay(d.day, kept))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ForgetPairsTx clears each subject's reading of its content in q, keeping
// hidden rows hidden: an imported removal.
func (s *Store) ForgetPairsTx(ctx context.Context, q execer, subjects, contents []uuid.UUID) error {
	if len(subjects) == 0 {
		return nil
	}
	for _, sql := range []string{
		`DELETE FROM ` + s.t.state + ` r USING unnest($2::uuid[], $3::uuid[]) AS u(subject, content_id)
 WHERE r.app_id = $1 AND r.subject = u.subject AND r.content_id = u.content_id AND NOT r.hidden`,
		`UPDATE ` + s.t.state + ` r SET first_day = NULL, last_seen_at = NULL, best_outcome = 0, furthest = 0, "position" = NULL
 FROM unnest($2::uuid[], $3::uuid[]) AS u(subject, content_id) WHERE r.app_id = $1 AND r.subject = u.subject AND r.content_id = u.content_id`,
	} {
		if _, err := q.Exec(ctx, sql, s.app, subjects, contents); err != nil {
			return err
		}
	}
	return nil
}

// ForgetContentsTx removes purged galleries from everyone's reading state in
// q. History keeps only their ids.
func (s *Store) ForgetContentsTx(ctx context.Context, q execer, contents []uuid.UUID) error {
	if len(contents) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `DELETE FROM `+s.t.state+` WHERE app_id = $1 AND content_id = ANY($2)`, s.app, contents)
	return err
}

// Erase deletes an account's reading state, history and links, and those
// of the anonymous ids folded into it, which it records as erased so their
// late rows never count again. anonymous is from Anonymous.
func (s *Store) Erase(ctx context.Context, account uuid.UUID, anonymous []uuid.UUID) error {
	subjects := append([]uuid.UUID{account}, anonymous...)
	texts := make([]string, len(anonymous))
	for i, a := range anonymous {
		texts[i] = a.String()
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO ` + s.t.erased + ` (tenant_id, actor_id, engine_cleanup_complete) SELECT $1, unnest($2::text[]), true ON CONFLICT (tenant_id, actor_id) DO NOTHING`, []any{s.tenant, texts}},
			{`DELETE FROM ` + s.t.state + ` WHERE app_id = $1 AND subject = ANY($2)`, []any{s.app, subjects}},
			{`DELETE FROM ` + s.t.history + ` WHERE app_id = $1 AND subject = ANY($2)`, []any{s.app, subjects}},
			{`DELETE FROM ` + s.t.links + ` WHERE app_id = $1 AND (account_id = $2 OR anonymous_id = ANY($3))`, []any{s.app, account, subjects}},
			{`DELETE FROM ` + s.t.active + ` WHERE app_id = $1 AND subject = ANY($2)`, []any{s.app, subjects}},
			{`UPDATE ` + s.t.reactions + ` SET anonymous_subject = NULL WHERE tenant_id = $1 AND anonymous_subject = ANY($2)`, []any{s.tenant, subjects}},
			{`DELETE FROM ` + s.t.searches + ` WHERE tenant_id = $1 AND subject = ANY($2)`, []any{s.tenant, texts}},
			// Feedback of the anonymous ids folded in; the account's own goes with its interactions.
			{`DELETE FROM ` + s.t.feedback + ` WHERE app_id = $1 AND anonymous_actor_id = ANY($2)`, []any{s.app, anonymous}},
		} {
			if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
				return fmt.Errorf("readingstore: erase: %w", err)
			}
		}
		return nil
	})
}

// Erased reports which subjects a recorded erasure covers.
func (s *Store) Erased(ctx context.Context, subjects []uuid.UUID) (map[uuid.UUID]bool, error) {
	texts := make([]string, len(subjects))
	for i, id := range subjects {
		texts[i] = id.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT actor_id::uuid FROM `+s.t.erased+` WHERE tenant_id = $1 AND actor_id = ANY($2)`, s.tenant, texts)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// JobProgress is where the analytics job resumes. ErasuresThrough is the
// newest erasure the daily delete has handled, by its own clock.
type JobProgress struct {
	IngestedThrough, RolledUpAt, ClosedAt, ErasedAt, ErasuresThrough *time.Time
}

// Progress reads the job's progress; zero before its first run.
func (s *Store) Progress(ctx context.Context) (JobProgress, error) {
	var p JobProgress
	err := s.pool.QueryRow(ctx, `SELECT ingested_through, rolled_up_at, closed_at, erased_at, erasures_through FROM `+s.t.job+` WHERE app_id = $1`, s.app).Scan(&p.IngestedThrough, &p.RolledUpAt, &p.ClosedAt, &p.ErasedAt, &p.ErasuresThrough)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobProgress{}, nil
	}
	return p, err
}

// SaveProgress records the job's progress.
func (s *Store) SaveProgress(ctx context.Context, p JobProgress) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO `+s.t.job+` (app_id, ingested_through, rolled_up_at, closed_at, erased_at, erasures_through) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (app_id) DO UPDATE SET ingested_through = EXCLUDED.ingested_through, rolled_up_at = EXCLUDED.rolled_up_at,
 closed_at = EXCLUDED.closed_at, erased_at = EXCLUDED.erased_at, erasures_through = EXCLUDED.erasures_through`,
		s.app, p.IngestedThrough, p.RolledUpAt, p.ClosedAt, p.ErasedAt, p.ErasuresThrough)
	return err
}

func clampInt32(v uint32) int32 { return int32(min(v, 1<<31-1)) }

func nonNil(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}
