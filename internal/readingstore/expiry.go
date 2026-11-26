package readingstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/subject"
)

// Idle signed-out subjects are expired expireBatch per transaction, at most
// expireBatches per run; a backlog continues on the next run.
const (
	expireBatch   = 500
	expireBatches = 20
)

// Active records signed-out subjects' latest activity from the log, keeping
// the later.
func (s *Store) Active(ctx context.Context, last map[uuid.UUID]time.Time) error {
	if len(last) == 0 {
		return nil
	}
	subjects := make([]uuid.UUID, 0, len(last))
	at := make([]time.Time, 0, len(last))
	for id, t := range last {
		subjects, at = append(subjects, id), append(at, t)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO `+s.t.active+` AS a (app_id, subject, last_active_at) SELECT $1, unnest($2::uuid[]), unnest($3::timestamptz[])
ON CONFLICT (app_id, subject) DO UPDATE SET last_active_at = GREATEST(a.last_active_at, EXCLUDED.last_active_at)`, s.app, subjects, at)
	return err
}

// Touch records a write of a signed-out subject's own rows (a search, a
// reaction, a hide, a feedback answer) as activity now, by the engine's clock, so expiry holds
// without the log. A row touched within the hour is left alone. Accounts are
// ignored.
func (s *Store) Touch(ctx context.Context, db execer, who uuid.UUID) error {
	if !subject.IsAnonymous(who) {
		return nil
	}
	_, err := db.Exec(ctx, `INSERT INTO `+s.t.active+` AS a (app_id, subject, last_active_at) VALUES ($1, $2, $3)
ON CONFLICT (app_id, subject) DO UPDATE SET last_active_at = EXCLUDED.last_active_at WHERE a.last_active_at < EXCLUDED.last_active_at - interval '1 hour'`, s.app, who, s.now())
	return err
}

// ExpireIdle expires signed-out subjects idle since before cutoff, a bounded
// number per call. It returns how many it expired.
func (s *Store) ExpireIdle(ctx context.Context, cutoff time.Time) (int, error) {
	total := 0
	for range expireBatches {
		n, err := s.expireBatch(ctx, cutoff)
		total += n
		if err != nil || n < expireBatch {
			return total, err
		}
	}
	return total, nil
}

// expireBatch deletes the personal rows of up to expireBatch signed-out
// subjects idle since before cutoff: reading state, history, recent searches
// and feedback answers, free text included; their reactions stay, unlinked
// from them. A subject linked to an
// account is never expired.
func (s *Store) expireBatch(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM `+s.t.active+` a WHERE a.app_id = $1 AND a.subject IN (
 SELECT i.subject FROM `+s.t.active+` i WHERE i.app_id = $1 AND i.last_active_at < $2
  AND NOT EXISTS (SELECT 1 FROM `+s.t.links+` l WHERE l.app_id = i.app_id AND l.anonymous_id = i.subject)
 ORDER BY i.last_active_at LIMIT $3 FOR UPDATE SKIP LOCKED) RETURNING a.subject`, s.app, cutoff, expireBatch)
		if err != nil {
			return err
		}
		subjects, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil || len(subjects) == 0 {
			return err
		}
		texts := make([]string, len(subjects))
		for i, id := range subjects {
			texts[i] = id.String()
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM ` + s.t.state + ` WHERE app_id = $1 AND subject = ANY($2)`, []any{s.app, subjects}},
			{`DELETE FROM ` + s.t.history + ` WHERE app_id = $1 AND subject = ANY($2)`, []any{s.app, subjects}},
			{`DELETE FROM ` + s.t.searches + ` WHERE tenant_id = $1 AND subject = ANY($2)`, []any{s.tenant, texts}},
			{`UPDATE ` + s.t.reactions + ` SET anonymous_subject = NULL WHERE tenant_id = $1 AND anonymous_subject = ANY($2)`, []any{s.tenant, subjects}},
			{`DELETE FROM ` + s.t.feedback + ` WHERE app_id = $1 AND anonymous_actor_id = ANY($2)`, []any{s.app, subjects}},
		} {
			if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
				return fmt.Errorf("readingstore: expire anonymous: %w", err)
			}
		}
		n = len(subjects)
		return nil
	})
	return n, err
}
