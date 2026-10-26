package authkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
)

// MigrationScopes supplies MigratePostgres with the same app-isolated group
// identities as AppScopes, without constructing an AuthKit runtime. The host
// supplies its normal AuthKit store/role configuration after migrating AuthKit.
// No signing keys, login providers, senders or worker fleet are needed.
func MigrationScopes(pool *pgxpool.Pool, cfg authkit.Config, scope contentkit.Scope) (entities.ScopeProvisioner, error) {
	if pool == nil {
		return nil, errors.New("contentkit/authkit: migration scopes require PostgreSQL")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return migrationScopes{pool: pool, cfg: cfg, scope: scope}, nil
}

type migrationScopes struct {
	pool  *pgxpool.Pool
	cfg   authkit.Config
	scope contentkit.Scope
}

func (s migrationScopes) ContentKitScope() (string, string) {
	return s.scope.TenantID.String(), s.scope.AppID.String()
}

func (s migrationScopes) Provision(ctx context.Context, scopes []entities.Scope) error {
	groups := make([]iam.NewGroup, 0, len(scopes))
	tenant, err := iam.ParseTenantID(s.scope.TenantID.String())
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if scope.Kind != "channel" {
			return errors.New("contentkit/authkit: configured authorization groups are only for channels")
		}
		if scope.Owner != "" || scope.Restore {
			return errors.New("contentkit/authkit: configured groups cannot assign owners or restore retired groups")
		}
		id, err := ChannelScopeID(s.scope, scope.ID)
		if err != nil {
			return err
		}
		var persona iam.Persona
		if err := persona.UnmarshalText([]byte(scope.Kind)); err != nil {
			return err
		}
		groups = append(groups, iam.NewGroup{ID: id, TenantID: tenant, Persona: persona})
	}
	if _, err := authkit.EnsureGroups(ctx, s.pool, s.cfg, groups); err != nil {
		return fmt.Errorf("contentkit/authkit: provision configured scopes: %w", err)
	}
	return nil
}
