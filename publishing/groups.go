package publishing

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/codes"
)

func (s *Store) resolveGroups(ctx context.Context, tx pgx.Tx, writer *groups.Store, refs []EntityReference, create bool) ([]string, []EntityResolution, error) {
	// Only taxonomy resolution is serialized, in this catalog. The transaction
	// does no storage I/O; concurrent submissions cannot mint duplicate names.
	if len(refs) > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "publishing:groups:"+s.o.Tenant); err != nil {
			return nil, nil, err
		}
	}
	groupTable := pgx.Identifier{s.o.Schema, "content_groups"}.Sanitize()
	nameTable := pgx.Identifier{s.o.Schema, "content_group_names"}.Sanitize()
	createdIDs := map[string]bool{}
	var resolve func(EntityReference, string) (EntityResolution, error)
	resolve = func(ref EntityReference, field string) (EntityResolution, error) {
		var scope *EntityResolution
		if ref.Scope != nil {
			parent, err := resolve(*ref.Scope, field+".scope")
			if err != nil {
				return EntityResolution{}, err
			}
			scope = &parent
		}
		scopeID := ""
		if scope != nil {
			scopeID = scope.ID
		}
		id, kind, storedScope := ref.ID, ref.Kind, ""
		if id != "" {
			err := tx.QueryRow(ctx, `SELECT kind,scope_id FROM `+groupTable+` WHERE tenant_id=$1 AND id=$2 AND state='active' FOR SHARE`, s.o.Tenant, id).Scan(&kind, &storedScope)
			if err == pgx.ErrNoRows {
				return EntityResolution{}, failure("unknown_entity", field, "entity does not exist in this catalog")
			}
			if err != nil {
				return EntityResolution{}, err
			}
			if ref.Kind != "" && ref.Kind != kind || scope != nil && storedScope != scopeID {
				return EntityResolution{}, failure("invalid_request", field, "entity kind or scope does not match")
			}
			if scope == nil && storedScope != "" {
				parent, err := resolve(EntityReference{ID: storedScope}, field+".scope")
				if err != nil {
					return EntityResolution{}, err
				}
				scope = &parent
			}
		} else {
			gk, ok := s.o.Catalog.Kinds().GroupKinds[kind]
			if !ok {
				return EntityResolution{}, failure("invalid_request", field+".kind", "entity kind is not configured")
			}
			if gk.ScopedBy != "" && scope == nil || gk.ScopedBy == "" && scope != nil || scope != nil && scope.Kind != gk.ScopedBy {
				return EntityResolution{}, failure("invalid_request", field+".scope", "entity scope does not match the configured kind")
			}
			ids, err := s.namedEntityIDs(ctx, tx, kind, scopeID, ref.Language, ref.Name)
			if err != nil {
				return EntityResolution{}, err
			}
			if len(ids) > 1 {
				return EntityResolution{}, failure("ambiguous_entity", field, "multiple entities match this name or alias; supply an id")
			}
			if len(ids) == 1 {
				id = ids[0]
			} else {
				if !create {
					return EntityResolution{}, failure("unknown_entity", field, "entity was not found; supply an id or enable create_missing_entities")
				}
				if gk.Owner == "user" || gk.AuthScope {
					return EntityResolution{}, failure("invalid_request", field, "ownership and authorization entities require explicit creation")
				}
				slug := codes.Slugify(ref.Name)
				if slug == "" {
					slug = "group-" + uuid.NewString()[:8]
				}
				var taken bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+groupTable+` WHERE tenant_id=$1 AND kind=$2 AND scope_id=$3 AND slug=$4 AND state='active')`, s.o.Tenant, kind, scopeID, slug).Scan(&taken); err != nil {
					return EntityResolution{}, err
				}
				if taken {
					slug += "-" + uuid.NewString()[:8]
				}
				made, err := writer.CreateGroupsTx(ctx, tx, []groups.GroupInput{{Kind: kind, Slug: slug, ScopeID: groups.GroupID(scopeID), Names: []groups.Name{{Language: ref.Language, Kind: groups.NameCanonical, Name: ref.Name}}}})
				if err != nil {
					return EntityResolution{}, err
				}
				id = string(made[0].ID)
				createdIDs[id] = true
			}
		}
		language := ref.Language
		if language == "" {
			language = s.o.Catalog.Kinds().Languages[0]
		}
		var name string
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT name FROM `+nameTable+` WHERE tenant_id=$1 AND group_id=$2 AND kind='name' ORDER BY language=$3 DESC,language LIMIT 1),$2)`, s.o.Tenant, id, language).Scan(&name); err != nil {
			return EntityResolution{}, err
		}
		return EntityResolution{ID: id, Kind: kind, Name: name, Created: createdIDs[id], Scope: scope}, nil
	}
	ids := []string{}
	entities := []EntityResolution{}
	seen := map[string]bool{}
	for i, ref := range refs {
		entity, err := resolve(ref, fmt.Sprintf("entities[%d]", i))
		if err != nil {
			return nil, nil, err
		}
		entities = append(entities, entity)
		if !seen[entity.ID] {
			seen[entity.ID] = true
			ids = append(ids, entity.ID)
		}
	}
	return ids, entities, nil
}
