package contentkit

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
)

// MigrateFixedOwners explicitly seeds configured site channels and assigns
// unowned content after MigratePostgres. It uses the same normalized app scope
// and schema as runtime, without starting AuthKit, billing, media or workers.
// Stop writers before changing a kind's ownership policy. Existing paid or
// differently owned content is refused, never silently made public/reassigned.
func MigrateFixedOwners(ctx context.Context, pool *pgxpool.Pool, cfg Config, seeds []entities.EntityInput) (int, error) {
	e := &Engine{cfg: cfg, pool: pool}
	if err := e.configureStorage(); err != nil {
		return 0, err
	}
	return catalog.InitializeFixedOwners(ctx, pool, e.schema, e.cfg.Tenant, e.cfg.Content, seeds)
}
