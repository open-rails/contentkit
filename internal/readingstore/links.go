package readingstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Link is an anonymous id signed in as an account.
type Link struct{ Anonymous, Account uuid.UUID }

// Link records that anonymous signed in as account. The first link of an
// anonymous id stands; an erased account takes none.
func (s *Store) Link(ctx context.Context, anonymous, account uuid.UUID) error {
	if anonymous == account {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO `+s.t.links+` (app_id, anonymous_id, account_id) SELECT $2::uuid, $3::uuid, $4::uuid WHERE `+s.notErased("$4::uuid")+`
ON CONFLICT (app_id, anonymous_id) DO NOTHING`, s.tenant, s.app, anonymous, account)
	return err
}

// Accounts maps those of subjects that are linked anonymous ids to their
// accounts.
func (s *Store) Accounts(ctx context.Context, subjects []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(subjects) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT anonymous_id, account_id FROM `+s.t.links+` WHERE app_id = $1 AND anonymous_id = ANY($2)`, s.app, subjects)
	if err != nil {
		return nil, err
	}
	links, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Link])
	for _, l := range links {
		out[l.Anonymous] = l.Account
	}
	return out, err
}

// Anonymous lists the anonymous ids linked to each of accounts.
func (s *Store) Anonymous(ctx context.Context, accounts []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	out := map[uuid.UUID][]uuid.UUID{}
	if len(accounts) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT anonymous_id, account_id FROM `+s.t.links+` WHERE app_id = $1 AND account_id = ANY($2) ORDER BY anonymous_id`, s.app, accounts)
	if err != nil {
		return nil, err
	}
	links, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Link])
	for _, l := range links {
		out[l.Account] = append(out[l.Account], l.Anonymous)
	}
	return out, err
}

// Unfolded lists links whose history the job has not folded yet, oldest first.
func (s *Store) Unfolded(ctx context.Context, limit int) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `SELECT anonymous_id, account_id FROM `+s.t.links+` WHERE app_id = $1 AND folded_at IS NULL ORDER BY linked_at, anonymous_id LIMIT $2`, s.app, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Link])
}

// Fold moves an anonymous id's reading into its account: reading_state and
// interaction_history merge, and its reactions and recent searches are
// re-keyed where the account has none; a reaction the account already has
// for the same target wins, and the anonymous one is unlinked. Repeating it
// is harmless.
func (s *Store) Fold(ctx context.Context, l Link) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var erased bool
		if err := tx.QueryRow(ctx, `SELECT NOT (`+s.notErased("$2::uuid")+`)`, s.tenant, l.Account).Scan(&erased); err != nil {
			return err
		}
		if !erased {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.state+` AS r (app_id, subject, content_id, first_day, last_seen_at, best_outcome, furthest, "position", hidden)
SELECT app_id, $3::uuid, content_id, first_day, last_seen_at, best_outcome, furthest, "position", hidden FROM `+s.t.state+` WHERE app_id = $1 AND subject = $2
ON CONFLICT (app_id, subject, content_id) DO UPDATE SET `+mergeSet+`, hidden = r.hidden OR EXCLUDED.hidden`, s.app, l.Anonymous, l.Account); err != nil {
				return fmt.Errorf("readingstore: fold reading state: %w", err)
			}
			if err := s.foldHistory(ctx, tx, l); err != nil {
				return err
			}
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`UPDATE ` + s.t.reactions + ` r SET user_id = $3, anonymous_subject = NULL WHERE r.tenant_id = $1 AND r.anonymous_subject = $2
 AND NOT EXISTS (SELECT 1 FROM ` + s.t.reactions + ` a WHERE a.tenant_id = r.tenant_id AND a.user_id = $3 AND a.content_kind = r.content_kind
  AND a.content_id = r.content_id AND a.content_version_id IS NOT DISTINCT FROM r.content_version_id)`, []any{s.tenant, l.Anonymous, l.Account.String()}},
				{s.unlinkReactions("$2"), []any{s.tenant, l.Anonymous}},
				{`UPDATE ` + s.t.searches + ` x SET subject = $3 WHERE x.tenant_id = $1 AND x.subject = $2
 AND NOT EXISTS (SELECT 1 FROM ` + s.t.searches + ` a WHERE a.tenant_id = x.tenant_id AND a.subject = $3 AND a.normalized = x.normalized)`,
					[]any{s.tenant, l.Anonymous.String(), l.Account.String()}},
			} {
				if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
					return fmt.Errorf("readingstore: re-key: %w", err)
				}
			}
		}
		for _, q := range []string{
			`DELETE FROM ` + s.t.state + ` WHERE app_id = $1 AND subject = $2`,
			`DELETE FROM ` + s.t.history + ` WHERE app_id = $1 AND subject = $2`,
			`DELETE FROM ` + s.t.active + ` WHERE app_id = $1 AND subject = $2`,
			`UPDATE ` + s.t.links + ` SET folded_at = now() WHERE app_id = $1 AND anonymous_id = $2`,
		} {
			if _, err := tx.Exec(ctx, q, s.app, l.Anonymous); err != nil {
				return err
			}
		}
		return nil
	})
}

// foldHistory merges the anonymous id's days into the account's, action by
// action in time order.
func (s *Store) foldHistory(ctx context.Context, tx pgx.Tx, l Link) error {
	rows, err := tx.Query(ctx, `SELECT a.day, a.actions, h.actions FROM `+s.t.history+` a LEFT JOIN `+s.t.history+` h
 ON h.app_id = a.app_id AND h.subject = $3 AND h.day = a.day WHERE a.app_id = $1 AND a.subject = $2`, s.app, l.Anonymous, l.Account)
	if err != nil {
		return err
	}
	type pair struct {
		day      time.Time
		anon, to []byte
	}
	pairs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pair, error) {
		var p pair
		err := r.Scan(&p.day, &p.anon, &p.to)
		return p, err
	})
	if err != nil {
		return err
	}
	for _, p := range pairs {
		merged, err := DecodeDay(p.day, p.anon)
		if err != nil {
			return err
		}
		if p.to != nil {
			own, err := DecodeDay(p.day, p.to)
			if err != nil {
				return err
			}
			merged = MergeActions(own, merged)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.history+` (app_id, subject, day, actions) VALUES ($1, $2, $3, $4)
ON CONFLICT (app_id, subject, day) DO UPDATE SET actions = EXCLUDED.actions`, s.app, l.Account, p.day, EncodeDay(p.day, merged)); err != nil {
			return err
		}
	}
	return nil
}

// MergeActions merges two people's records of one day: in time order, one
// action per gallery at its first, keeping the most read, the longest time
// and the latest reaction.
func MergeActions(a, b []Action) []Action {
	all := append(slices.Clone(a), b...)
	slices.SortStableFunc(all, func(x, y Action) int { return x.At.Compare(y.At) })
	out := make([]Action, 0, len(all))
	first := map[uuid.UUID]int{}
	for _, act := range all {
		if act.Kind != ActionGallery {
			out = append(out, act)
			continue
		}
		i, ok := first[act.ID]
		if !ok {
			first[act.ID] = len(out)
			out = append(out, act)
			continue
		}
		kept := &out[i]
		kept.Tenths, kept.ActiveS = max(kept.Tenths, act.Tenths), max(kept.ActiveS, act.ActiveS)
		if act.Reaction != ReactionNone {
			kept.Reaction = act.Reaction
		}
	}
	return out
}
