package versionstore

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	versions "github.com/open-rails/contentkit/media/versions"
	"golang.org/x/sync/errgroup"
)

// SyncFences makes each target version's gateway fence follow the catalog:
// a version deleted itself or with its content serves nothing until restored.
// Each content's rows stay share-locked through the storage calls, so a
// deletion, restore or erasure claim commits before or after a sync, never
// during one. Catalog writes sync after they commit; the publication job the
// same write scheduled syncs again, so a crash cannot leave the fence stale.
func (s *Service) SyncFences(ctx context.Context, targets []versions.Target) error {
	if s.reg == nil || s.m.Jobs == nil {
		return nil
	}
	byContent := map[string][]string{} // nil: every version
	for _, t := range targets {
		vers, seen := byContent[t.Content]
		switch {
		case t.Version == "":
			byContent[t.Content] = nil
		case !seen || vers != nil:
			byContent[t.Content] = append(vers, t.Version)
		}
	}
	var failures error
	for _, content := range slices.Sorted(maps.Keys(byContent)) {
		failures = errors.Join(failures, pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
			return s.syncFencesTx(ctx, tx, content, byContent[content])
		}))
	}
	return failures
}

func (s *Service) syncFencesTx(ctx context.Context, tx pgx.Tx, content string, vers []string) error {
	var kind string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT kind,state='deleted' FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2 FOR SHARE`,
		s.o.Tenant, content).Scan(&kind, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // purged: erasure fenced its versions for good
	} else if err != nil {
		return err
	}
	if k, err := s.reg.Kind(kind); err != nil || !k.Versions {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,state='deleted' FROM `+s.t.versions+`
 WHERE tenant_id=$1 AND content_id=$2 AND ($3::text[] IS NULL OR id=ANY($3)) ORDER BY id FOR SHARE`, s.o.Tenant, content, vers)
	if err != nil {
		return err
	}
	type version struct {
		ID      string
		Deleted bool
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[version])
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, v := range found {
		g.Go(func() error {
			ref, err := s.reg.VersionRef(kind, content, v.ID)
			if err != nil {
				return err
			}
			return s.m.Jobs.FenceDeleted(gctx, ref, deleted || v.Deleted)
		})
	}
	return g.Wait()
}
