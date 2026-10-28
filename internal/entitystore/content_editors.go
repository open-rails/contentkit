package entitystore

import (
	"context"
	"slices"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	entities "github.com/open-rails/contentkit/entities"
)

// ContentEditorEntities lists only this actor's owned or delegated content-writing
// entities. The scope directory is narrowed to this app and credential ceilings
// by heldScopes, as it is for Mine. It never enumerates catalog content.
func (s *Store) ContentEditorEntities(ctx context.Context, a auth.Authenticator, actor access.Actor) ([]entities.EntityID, error) {
	return s.contentAuthorityEntities(ctx, a, actor, func(kind string) []string { return []string{access.ChannelContentWrite} })
}

// ContentAuthorEntities returns channels where the current credential may read
// or edit its own posts. This does not confer authority over other authors.
func (s *Store) ContentAuthorEntities(ctx context.Context, a auth.Authenticator, actor access.Actor, editing bool) ([]entities.EntityID, error) {
	return s.contentAuthorityEntities(ctx, a, actor, func(kind string) []string {
		permissions := []string{access.ChannelContentEditOwn, access.ChannelContentWrite}
		if !editing {
			permissions = append(permissions, access.ChannelContentCreate)
		}
		return permissions
	})
}

func (s *Store) contentAuthorityEntities(ctx context.Context, a auth.Authenticator, actor access.Actor, permissions func(string) []string) ([]entities.EntityID, error) {
	out := []entities.EntityID{}
	if !actor.Member() {
		return out, nil
	}
	bound := s.As(a, actor)
	held, err := bound.heldScopes(ctx, actor, "")
	if err != nil {
		return nil, err
	}
	scopeIDs := make([]string, 0, len(held))
	for scope := range held {
		scopeIDs = append(scopeIDs, scope)
	}
	err = s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, `SELECT id,kind,coalesce(auth_scope,''),coalesce(owner_user_id=$2,false) FROM `+s.table("content_entities")+`
 WHERE tenant_id=$1 AND kind='channel' AND state='active' AND auth_scope IS NOT NULL AND (owner_user_id=$2 OR auth_scope=ANY($3::text[])) ORDER BY id`, s.tenant, actor.AccountID(), scopeIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id entities.EntityID
			var kind, scope string
			var owner bool
			if err := rows.Scan(&id, &kind, &scope, &owner); err != nil {
				return err
			}
			if slices.ContainsFunc(permissions(kind), func(permission string) bool { return slices.Contains(held[scope], permission) }) {
				out = append(out, id)
			}
		}
		return rows.Err()
	})
	return out, err
}
