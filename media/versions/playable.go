package versions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
)

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
