package versions

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// WithChapterLocks settles admitted media changes before catalog marker
// authoring. The caller holds source locks first and takes catalog rows inside
// fn; uploads/recovery take manifest then catalog locks in the same order.
func (s *Service) WithChapterLocks(ctx context.Context, targets []Target, fn func() error) error {
	vs, err := s.versions(ctx, s.o.Pool, targets)
	if err != nil {
		return err
	}
	refs := map[string]contentref.ContentRef{}
	for _, target := range targets {
		v, ok := vs[target]
		if !ok {
			return ErrNotFound
		}
		ref, err := s.ref(v.Kind, target)
		if err != nil {
			return err
		}
		item, err := s.reg.Item(ref)
		if err != nil {
			return err
		}
		refs[item.Prefix()] = ref
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var locked func(int) error
	locked = func(index int) error {
		if index == len(keys) {
			return fn()
		}
		return s.m.Manifests.WithRecoveredLock(ctx, refs[keys[index]], func() error { return locked(index + 1) })
	}
	return locked(0)
}

func validateChapterPaths(ctx context.Context, q querier, table, tenant, version string, renames map[string]string, removed []string) error {
	if len(removed) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT start_page FROM `+table+` WHERE tenant_id=$1 AND version_id=$2 AND start_page IS NOT NULL`, tenant, version)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var marker string
		if err := rows.Scan(&marker); err != nil {
			return err
		}
		if next, ok := renames[marker]; ok {
			marker = next
		}
		if slices.Contains(removed, marker) {
			return fmt.Errorf("%w: choose a new chapter start before removing %q", ErrInvalid, marker)
		}
	}
	return rows.Err()
}

func applyChapterPaths(ctx context.Context, tx pgx.Tx, table, tenant, version string, renames map[string]string, removed []string) error {
	if err := validateChapterPaths(ctx, tx, table, tenant, version, renames, removed); err != nil {
		return err
	}
	if len(renames) == 0 {
		return nil
	}
	from := make([]string, 0, len(renames))
	for path := range renames {
		from = append(from, path)
	}
	slices.Sort(from)
	to := make([]string, len(from))
	for i, path := range from {
		to[i] = renames[path]
	}
	rows, err := tx.Query(ctx, `SELECT c.id,r.new_path FROM `+table+` c
 JOIN unnest($3::text[],$4::text[]) AS r(old_path,new_path) ON c.start_page=r.old_path
 WHERE c.tenant_id=$1 AND c.version_id=$2 FOR UPDATE OF c`, tenant, version, from, to)
	if err != nil {
		return err
	}
	type marker struct{ ID, Path string }
	markers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[marker])
	if err != nil || len(markers) == 0 {
		return err
	}
	ids, paths := make([]string, len(markers)), make([]string, len(markers))
	for i, m := range markers {
		ids[i], paths[i] = m.ID, m.Path
	}
	// Release marker-name uniqueness within this transaction so a batch can
	// swap names without creating a transient duplicate. Numeric bounds are
	// only a temporary valid storage shape; authored markers remain paths.
	if _, err := tx.Exec(ctx, `UPDATE `+table+` SET start_page=NULL,start_index=0,end_index=1
 WHERE tenant_id=$1 AND version_id=$2 AND id=ANY($3)`, tenant, version, ids); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE `+table+` c SET start_page=r.path,start_index=NULL,end_index=NULL
 FROM unnest($3::text[],$4::text[]) AS r(id,path)
 WHERE c.tenant_id=$1 AND c.version_id=$2 AND c.id=r.id`, tenant, version, ids, paths)
	return err
}

func (s *Service) validatePathsChanged(ctx context.Context, ref contentref.ContentRef, renames map[string]string, removed []string) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions {
		return err
	}
	return validateChapterPaths(ctx, s.o.Pool, s.t.segments, s.o.Tenant, ref.Version(), renames, removed)
}

func (s *Service) pathsChanged(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, renames map[string]string, removed []string) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions {
		return err
	}
	if err := applyChapterPaths(ctx, tx, s.t.segments, s.o.Tenant, ref.Version(), renames, removed); err != nil {
		return err
	}
	return s.SchedulePublicationTx(ctx, tx, []Target{{Content: ref.ContentID, Version: ref.Version()}})
}

// ChapterPathHooks installs the same marker contract in stock worker recovery.
// The journal must not settle a recovered rename without its catalog update.
func ChapterPathHooks(pool *pgxpool.Pool, schema string, registry *media.Registry, wake func(context.Context, pgx.Tx, contentref.ContentRef) error) media.Hooks {
	table := pgxIdent(schema, "content_segments")
	tenant := func(ctx context.Context, q querier, ref contentref.ContentRef) (string, error) {
		kind, err := registry.Kind(ref.ContentKind)
		if err != nil || !kind.Versions {
			return "", err
		}
		if _, err := registry.Item(ref); err != nil {
			return "", err
		}
		partition := ref.TenantID
		if scope, scoped := layout.ScopeFromPartition(partition); scoped {
			if err := q.QueryRow(ctx, `SELECT catalog_key FROM `+pgxIdent(schema, "content_app_bindings")+` WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&partition); err != nil {
				return "", err
			}
		}
		return partition, nil
	}
	return media.Hooks{
		ValidatePathsChanged: func(ctx context.Context, ref contentref.ContentRef, renames map[string]string, removed []string) error {
			partition, err := tenant(ctx, pool, ref)
			if err != nil || partition == "" {
				return err
			}
			return validateChapterPaths(ctx, pool, table, partition, ref.Version(), renames, removed)
		},
		PathsChanged: func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, renames map[string]string, removed []string) error {
			partition, err := tenant(ctx, tx, ref)
			if err != nil || partition == "" {
				return err
			}
			if err := applyChapterPaths(ctx, tx, table, partition, ref.Version(), renames, removed); err != nil {
				return err
			}
			if wake == nil {
				return errors.New("versions: chapter mutation needs durable publisher")
			}
			return wake(ctx, tx, ref)
		},
	}
}
