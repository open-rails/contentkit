package versions

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
)

// playableArgs re-reads, daily, player versions not known to play: those
// from before the fact existed (unknown lists, so this only hides those that
// do not play) and those whose media arrived past the hooks.
type playableArgs struct{}

func (playableArgs) Kind() string { return "contentkit_version_playable" }

type playableWorker struct {
	river.WorkerDefaults[playableArgs]
	s *Service
}

func (w *playableWorker) Timeout(*river.Job[playableArgs]) time.Duration { return 30 * time.Minute }

func (w *playableWorker) Work(ctx context.Context, job *river.Job[playableArgs]) error {
	return media.SnoozeUnavailable(ctx, w.s.m.Manifests.Store(), job.JobRow, w.s.recordPlayable(ctx))
}

const playableBatch = 100

// recordPlayable records the facts of every player version not known to
// play, which refreshes its listing when they changed.
func (s *Service) recordPlayable(ctx context.Context) error {
	var players []string
	for name, k := range s.o.Kinds.Kinds {
		if k.Presentation == "player" {
			players = append(players, name)
		}
	}
	if len(players) == 0 {
		return nil
	}
	slices.Sort(players)
	after := ""
	for {
		rows, err := s.o.Pool.Query(ctx, `SELECT c.kind, v.content_id, v.id FROM `+s.t.versions+` v
JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE v.tenant_id = $1 AND c.kind = ANY($2) AND v.playable IS NOT TRUE AND v.id > $3 ORDER BY v.id LIMIT $4`,
			s.o.Tenant, players, after, playableBatch)
		if err != nil {
			return err
		}
		type pending struct {
			kind string
			Target
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
			var p pending
			return p, row.Scan(&p.kind, &p.Content, &p.Version)
		})
		if err != nil {
			return err
		}
		for _, p := range batch {
			if err := s.refreshMediaFacts(ctx, p.kind, p.Target); err != nil {
				if errors.Is(err, media.ErrUnavailable) || ctx.Err() != nil {
					return err
				}
				// One version that cannot be read must not hold back the rest; it
				// stays unknown, which lists, and the next run retries it.
				s.o.Logger.WarnContext(ctx, "versions: cannot record whether a version plays", "content", p.Content, "version", p.Version, "error", err)
			}
		}
		if len(batch) < playableBatch {
			return nil
		}
		after = batch[len(batch)-1].Version
	}
}

func (s *Service) refreshMediaFacts(ctx context.Context, kind string, v Target) error {
	k, kindErr := s.reg.Kind(kind)
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := s.lockTarget(ctx, tx, v); err != nil {
			return err
		}
		if kindErr == nil {
			ref, err := s.ref(kind, v)
			if err != nil {
				return err
			}
			m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
			if err == nil {
				return s.recordFacts(ctx, tx, k, v, m)
			} else if !errors.Is(err, media.ErrNotFound) {
				return err
			}
		}
		if !s.player(kind) {
			return nil
		}
		// No media, or a kind without any: nothing plays.
		var id string
		err := tx.QueryRow(ctx, `UPDATE `+s.t.versions+` SET playable = false, updated_at = now()
WHERE tenant_id = $1 AND id = $2 AND content_id = $3 AND playable IS NULL RETURNING id`, s.o.Tenant, v.Version, v.Content).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		return s.o.Facets.RefreshTx(ctx, tx, []string{id})
	})
}
