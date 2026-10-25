package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// InitializeFixedOwners is an explicit trusted migration after catalog schema
// migrations. It seeds configured site channels and binds previously unowned
// content atomically. It never runs as part of startup or relaxes access rules.
// Existing channels retain their names and editor choices. Conflicting owners,
// deleted channels and content/version rules outside current host policy fail
// the entire transaction. Writers must be stopped during a policy migration.
func InitializeFixedOwners(ctx context.Context, pool *pgxpool.Pool, schema, tenant string, model kinds.Config, seeds []entities.EntityInput) (int, error) {
	if pool == nil || schema == "" || tenant == "" {
		return 0, errors.New("catalog: fixed-owner migration needs pool, schema and tenant")
	}
	if err := model.Validate(); err != nil {
		return 0, err
	}
	store, err := entities.New(entities.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: model})
	if err != nil {
		return 0, err
	}
	s := &Store{pool: pool, schema: schema, qs: pgx.Identifier{schema}.Sanitize(), tenant: tenant, cfg: model, t: catalogTables(schema), now: time.Now}
	refs := map[string]kinds.EntityRef{}
	var fixedKinds []string
	for kind, k := range model.Kinds {
		if k.OwnerEntity != nil {
			refs[k.OwnerEntity.Kind+"/"+k.OwnerEntity.Slug] = *k.OwnerEntity
			fixedKinds = append(fixedKinds, kind)
		}
	}
	slices.Sort(fixedKinds)
	inputs := slices.Clone(seeds)
	slices.SortFunc(inputs, func(a, b entities.EntityInput) int { return strings.Compare(a.Kind+"/"+a.Slug, b.Kind+"/"+b.Slug) })
	changed := 0
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// One transaction-scoped migration lock also serializes callers that provide
		// the same channel for several content kinds.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, schema+"/fixed-owners/"+tenant); err != nil {
			return err
		}
		for _, in := range inputs {
			if _, ok := refs[in.Kind+"/"+in.Slug]; !ok || in.OwnerUserID != "" || in.AuthScope != "" || in.ScopeID != "" || in.Internal {
				return invalid("fixed owner seed %s/%s is not a configured site channel", in.Kind, in.Slug)
			}
			var id, state string
			var valid bool
			err := tx.QueryRow(ctx, `SELECT id,state,owner_user_id IS NULL AND auth_scope IS NULL AND NOT internal FROM `+s.t.entities+`
 WHERE tenant_id=$1 AND kind=$2 AND slug=$3 AND scope_id='' ORDER BY state='active' DESC,id LIMIT 1 FOR UPDATE`, tenant, in.Kind, in.Slug).Scan(&id, &state, &valid)
			if err == nil {
				if state != "active" || !valid || in.ID != "" && string(in.ID) != id {
					return invalid("configured site channel %s/%s conflicts with stored ownership or state", in.Kind, in.Slug)
				}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if in.ID == "" {
				in.ID = entities.EntityID(contentref.NewID())
			}
			if err := store.ImportEntities(ctx, tx, []entities.ImportEntity{{ID: in.ID, Kind: in.Kind, Slug: in.Slug, State: entities.StateActive, Names: in.Names, Text: in.Text, Attributes: in.Attributes, Cover: in.Cover, Avatar: in.Avatar}}, false); err != nil {
				return err
			}
		}
		for _, kind := range fixedKinds {
			owner, err := s.fixedOwner(ctx, tx, kind, "")
			if err != nil {
				return err
			}
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
			if len(ids) == 0 {
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
			changed += len(ids)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}
