package versionstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media/layout"
)

// RetirementBarrier supplies the catalog-side media barrier to standalone
// workers. It shares the exact withdrawal logic used by embedded writers;
// workers need no authentication or application callbacks to retire a file.
func RetirementBarrier(pool *pgxpool.Pool, schema string, registry *mediastore.Registry) func(context.Context, contentref.ContentRef, *mediastore.Manifest) error {
	tables := tables{items: pgxIdent(schema, "content_items"), versions: pgxIdent(schema, "content_versions"), publications: pgxIdent(schema, "content_version_publications")}
	return func(ctx context.Context, ref contentref.ContentRef, current *mediastore.Manifest) error {
		kind, err := registry.Kind(ref.ContentKind)
		if err != nil || !kind.Versions || kind.Publish == nil || ref.Version() == "" {
			return nil
		}
		if _, err := registry.Item(ref); err != nil {
			return err
		}
		tenant := ref.TenantID
		if scope, scoped := layout.ScopeFromPartition(ref.TenantID); scoped {
			err = pool.QueryRow(ctx, `SELECT catalog_key FROM `+pgxIdent(schema, "content_app_bindings")+` WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&tenant)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("versions: media retirement has no catalog binding for %s", ref.TenantID)
			}
			if err != nil {
				return err
			}
		}
		s := Service{o: Options{Tenant: tenant, Pool: pool}, t: tables, reg: registry}
		return s.beforeRetire(ctx, ref, current)
	}
}
