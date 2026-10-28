package catalogstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/kinds"
)

// InitializeFixedOwners realizes the fixed owner_entity declarations during an
// explicit migration. It preserves existing entities, grants and access rules.
// Host writers must be stopped while changing ownership policy; concurrent
// migrators are serialized around their database writes.
//
// Entity IDs commit before external scope provisioning so an interrupted call
// can retry with the same identity. No database transaction or pooled connection
// is held during the provider call. Content is attached only after provisioning
// succeeds and the stored identities and policies have been checked again.
func InitializeFixedOwners(ctx context.Context, pool *pgxpool.Pool, schema, tenant string, model kinds.Config, scopes entities.ScopeProvisioner) error {
	refs := map[string]kinds.EntityRef{}
	var fixedKinds []string
	for kind, k := range model.Kinds {
		if k.Channel != nil && k.Channel.Fixed != "" {
			refs["channel/"+k.Channel.Fixed] = kinds.EntityRef{Kind: "channel", Slug: k.Channel.Fixed}
			fixedKinds = append(fixedKinds, kind)
		}
	}
	if err := validateChannelModel(ctx, pool, schema, tenant, model); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	if pool == nil || schema == "" || tenant == "" {
		return errors.New("catalog: fixed-owner provisioning needs pool, schema and tenant")
	}
	if err := model.Validate(); err != nil {
		return err
	}
	model = model.StorageModel()
	keys := make([]string, 0, len(refs))
	for key, ref := range refs {
		if model.EntityKinds[ref.Kind].AuthScope && scopes == nil {
			return invalid("configured site channel %s requires a Scopes provider", key)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	slices.Sort(fixedKinds)
	// This store only reserves entity rows. Migration itself owns synchronous
	// scope provisioning and retry, so it supplies no lifecycle queue adapter.
	store, err := entitystore.New(entitystore.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: model})
	if err != nil {
		return err
	}
	store = store.ChannelStorage()
	s := &Store{pool: pool, schema: schema, qs: pgx.Identifier{schema}.Sanitize(), tenant: tenant, cfg: model, t: catalogTables(schema), now: time.Now}
	owners := map[string]fixedOwnerIdentity{}
	locked := func(fn func(pgx.Tx) error) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, schema+"/fixed-owners/"+tenant); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	if err := locked(func(tx pgx.Tx) error {
		for _, key := range keys {
			ref := refs[key]
			owner, err := s.configuredOwner(ctx, tx, ref)
			if errors.Is(err, pgx.ErrNoRows) {
				owner.id = string(contentref.NewID())
				if err := store.ImportEntities(ctx, tx, []entities.ImportEntity{{
					ID: entities.EntityID(owner.id), Kind: ref.Kind, Slug: ref.Slug, State: entities.StateActive,
					Names: []entities.Name{{Language: model.Languages[0], Kind: entities.NameCanonical, Name: ref.Slug}},
				}}, true); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			owners[key] = owner
		}
		// Reject incompatible historical content before any new entity commits.
		return s.bindFixedOwnerContents(ctx, tx, model, fixedKinds, owners, false)
	}); err != nil {
		return err
	}
	var requested []entities.Scope
	for _, key := range keys {
		ref, owner := refs[key], owners[key]
		if model.EntityKinds[ref.Kind].AuthScope {
			requested = append(requested, entities.Scope{ID: owner.id, Kind: ref.Kind})
		}
	}
	if len(requested) > 0 {
		if err := scopes.Provision(ctx, requested); err != nil {
			return fmt.Errorf("catalog: provision configured channel scopes: %w", err)
		}
	}
	return locked(func(tx pgx.Tx) error {
		for _, key := range keys {
			ref := refs[key]
			current, err := s.configuredOwner(ctx, tx, ref)
			if err != nil {
				return err
			}
			if current.id != owners[key].id {
				return invalid("configured site channel %s changed while provisioning", key)
			}
			if model.EntityKinds[ref.Kind].AuthScope && current.scope == "" {
				if _, err := tx.Exec(ctx, `UPDATE `+s.t.entities+` SET auth_scope=id,revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, tenant, current.id); err != nil {
					return err
				}
			}
		}
		return s.bindFixedOwnerContents(ctx, tx, model, fixedKinds, owners, true)
	})
}

type fixedOwnerIdentity struct{ id, scope string }

func (s *Store) configuredOwner(ctx context.Context, tx pgx.Tx, ref kinds.EntityRef) (fixedOwnerIdentity, error) {
	var owner fixedOwnerIdentity
	var state string
	var valid bool
	err := tx.QueryRow(ctx, `SELECT id,state,coalesce(auth_scope,''),owner_user_id IS NULL AND NOT internal FROM `+s.t.entities+`
 WHERE tenant_id=$1 AND kind=$2 AND slug=$3 AND scope_id='' ORDER BY state='active' DESC,id LIMIT 1 FOR UPDATE`, s.tenant, ref.Kind, ref.Slug).Scan(&owner.id, &state, &owner.scope, &valid)
	if err != nil {
		return owner, err
	}
	if state != "active" || !valid || owner.scope != "" && owner.scope != owner.id {
		return owner, invalid("configured site channel %s/%s conflicts with stored ownership, scope or state", ref.Kind, ref.Slug)
	}
	return owner, nil
}

// bindFixedOwnerContents validates the complete existing policy before adopting
// unowned posts. The validation-only pass leaves incompatible catalogs untouched.
func (s *Store) bindFixedOwnerContents(ctx context.Context, tx pgx.Tx, model kinds.Config, fixedKinds []string, owners map[string]fixedOwnerIdentity, write bool) error {
	tenant := s.tenant
	for _, kind := range fixedKinds {
		ref := kinds.EntityRef{Kind: "channel", Slug: model.Kinds[kind].Channel.Fixed}
		owner := owners[ref.Kind+"/"+ref.Slug].id
		// Keep the original access policy. A formerly private blog must be resolved
		// explicitly rather than silently opened by this ownership migration.
		rows, err := tx.Query(ctx, `SELECT id,coalesce(owner_entity_id,''),access FROM `+s.t.items+` WHERE tenant_id=$1 AND kind=$2 ORDER BY id FOR UPDATE`, tenant, kind)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id, current string
			var rule access.Rule
			if err := rows.Scan(&id, &current, &rule); err != nil {
				rows.Close()
				return err
			}
			if current != "" && current != owner {
				rows.Close()
				return invalid("content %s already belongs to another channel", id)
			}
			if err := s.checkKindRule(kind, rule); err != nil {
				rows.Close()
				return fmt.Errorf("content %s: %w", id, err)
			}
			if current == "" {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT v.id,v.access FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE c.tenant_id=$1 AND c.kind=$2 AND v.access IS NOT NULL ORDER BY v.id FOR UPDATE OF v`, tenant, kind)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var raw json.RawMessage
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			var rule access.Rule
			if err := json.Unmarshal(raw, &rule); err != nil {
				rows.Close()
				return err
			}
			if err := s.checkKindRule(kind, rule); err != nil {
				rows.Close()
				return fmt.Errorf("version %s: %w", id, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !write || len(ids) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET owner_entity_id=$3,revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=ANY($2)`, tenant, ids, owner); err != nil {
			return err
		}
		// Ownership is independent from membership, release time and access facets.
		// Refresh only its projection, preserving all media/readiness fields.
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.facets+` SET owner_entity_id=$3,updated_at=clock_timestamp() WHERE tenant_id=$1 AND content_id=ANY($2)`, tenant, ids, owner); err != nil {
			return err
		}
		if err := s.markDocuments(ctx, tx, ids); err != nil {
			return err
		}
	}
	return nil
}

func validateChannelModel(ctx context.Context, pool *pgxpool.Pool, schema, tenant string, model kinds.Config) error {
	if model.Channels == nil {
		return nil
	}
	if pool == nil {
		return errors.New("catalog: channel migration requires a pool")
	}
	q := pgx.Identifier{schema}.Sanitize()
	for kind, k := range model.Kinds {
		if k.Channel == nil {
			continue
		}
		var bad string
		err := pool.QueryRow(ctx, `SELECT v.id FROM `+q+`.content_versions v JOIN `+q+`.content_items c ON c.tenant_id=v.tenant_id AND c.id=v.content_id WHERE c.tenant_id=$1 AND c.kind=$2 AND c.state<>'deleted' AND v.state<>'deleted' AND (c.owner_entity_id IS NOT NULL OR $3) AND v.access IS DISTINCT FROM c.access LIMIT 1`, tenant, kind, k.Channel.Fixed != "").Scan(&bad)
		if err == nil {
			return invalid("channel version %s has a distinct audience rule; resolve it before changing to shared parent access", bad)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}
