package entitystore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/open-rails/contentkit/access"
	entities "github.com/open-rails/contentkit/entities"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	"github.com/open-rails/contentkit/internal/gate"
)

// shown is one entity a page shows; fill completes it in place.
type shown struct {
	kind   string
	id     entities.EntityID
	cover  *entities.Cover
	avatar **entities.Avatar
	access **entities.EntityAccess
}

func cardShown(g *entities.EntityCard) shown {
	out := shown{kind: g.Kind, id: g.ID, cover: g.Cover, avatar: &g.Avatar}
	if !g.Internal {
		out.access = &g.Access
	}
	return out
}

// fill resolves the covers (contents' in language) and the user-owned
// entities' avatars of a page in one call, and the entitlement membership standing of
// the store's viewer, in one more.
func (s *Store) fill(ctx context.Context, list []shown, language string) error {
	var asked []entities.CoverOf
	var put []func([]entities.Rendition)
	var entitledIDs []string
	var standingAt []**entities.EntityAccess
	for _, sh := range list {
		gk := s.cfg.EntityKinds[sh.kind]
		if s.covers != nil && sh.cover != nil {
			c := sh.cover
			asked, put = append(asked, entities.CoverOf{Kind: sh.kind, ID: sh.id, Cover: *c, Language: language}), append(put, func(r []entities.Rendition) { c.Renditions = r })
		}
		if s.covers != nil && gk.HasAvatar() && sh.avatar != nil {
			a, image := sh.avatar, ""
			if *a != nil {
				image = (*a).Image
			}
			asked = append(asked, entities.CoverOf{Kind: sh.kind, ID: sh.id, Cover: entities.Cover{Image: image}, Preset: entities.AvatarPreset})
			put = append(put, func(r []entities.Rendition) {
				if len(r) > 0 && *a == nil {
					*a = &entities.Avatar{}
				}
				if *a != nil {
					(*a).Renditions = r
				}
			})
		}
		if s.gate != nil && gk.Entitled && sh.access != nil {
			entitledIDs, standingAt = append(entitledIDs, string(sh.id)), append(standingAt, sh.access)
		}
	}
	if len(asked) > 0 {
		got, err := s.covers.Covers(ctx, asked)
		if err != nil {
			return err
		}
		for i, f := range put {
			f(got[i])
		}
	}
	if len(entitledIDs) > 0 {
		keys := make([]access.Key, len(entitledIDs))
		for i, id := range entitledIDs {
			var err error
			keys[i], err = access.ChannelKey(s.tenant, id)
			if err != nil {
				return err
			}
		}
		held, err := s.gate.Holds(ctx, s.viewer(), keys)
		unknown := errors.Is(err, entitlement.ErrUnavailable)
		if err != nil && !unknown {
			return err
		}
		editors := s.contentEditors(ctx, entitledIDs)
		for i, at := range standingAt {
			*at = &entities.EntityAccess{Key: keys[i], Member: held[i], Editor: editors[entitledIDs[i]], Unknown: unknown}
		}
	}
	return nil
}

// contentEditors reports, per entity, whether the store's viewer edits its
// content: its owner, or a holder of {kind}:content:write in its AuthKit
// scope, read in one Scopes.Holding call for the page. A failed check reads
// as not an editor.
func (s *Store) contentEditors(ctx context.Context, ids []string) map[string]bool {
	out := map[string]bool{}
	v := s.viewer()
	if !v.Person() || len(ids) == 0 {
		return out
	}
	type entity struct{ id, kind, scope string }
	var scoped []entity
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, kind, coalesce(owner_user_id, '') = $3, coalesce(auth_scope, '') FROM %s
 WHERE tenant_id = $1 AND id = ANY($2::text[]) AND state = 'active'`, s.table("content_entities")), s.tenant, ids, v.AccountID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g entity
			var owner bool
			if err := rows.Scan(&g.id, &g.kind, &owner, &g.scope); err != nil {
				return err
			}
			if out[g.id] = owner && g.scope == "" && !s.cfg.EntityKinds[g.kind].AuthScope; g.scope != "" {
				scoped = append(scoped, g)
			}
		}
		return rows.Err()
	})
	if err == nil && len(scoped) > 0 && s.scopes != nil && v.Member() {
		var held map[string][]string
		if held, err = s.heldScopes(ctx, v, ""); err == nil {
			for _, g := range scoped {
				out[g.id] = slices.Contains(held[g.scope], access.ChannelContentWrite)
			}
		}
	} else if err == nil {
		for _, g := range scoped {
			if out[g.id], err = gate.ChannelEditor(ctx, s.as.auth, v, g.scope, access.ChannelContentWrite); err != nil {
				break
			}
		}
	}
	if err != nil {
		s.log.WarnContext(ctx, "entities: content editor check failed; reading as a non-editor", "err", err)
	}
	return out
}
