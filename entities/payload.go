package entities

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// AvatarPreset is the media preset avatars render in (media.AvatarPreset).
const AvatarPreset = "avatar"

// EntityAccess is an entity entitlement and the current viewer's standing.
// Product and price information belongs to the billing provider.
type EntityAccess struct {
	Key     access.Key `json:"key"`
	Member  bool       `json:"member"`
	Editor  bool       `json:"editor,omitempty"`
	Unknown bool       `json:"unknown,omitempty"`
}

// shown is one entity a page shows; fill completes it in place.
type shown struct {
	kind   string
	id     EntityID
	cover  *Cover
	avatar **Avatar
	access **EntityAccess
}

func cardShown(g *EntityCard) shown {
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
	var asked []CoverOf
	var put []func([]Rendition)
	var sold []string
	var soldAt []**EntityAccess
	for _, sh := range list {
		gk := s.cfg.EntityKinds[sh.kind]
		if s.covers != nil && sh.cover != nil {
			c := sh.cover
			asked, put = append(asked, CoverOf{Kind: sh.kind, ID: sh.id, Cover: *c, Language: language}), append(put, func(r []Rendition) { c.Renditions = r })
		}
		if s.covers != nil && gk.HasAvatar() && sh.avatar != nil {
			a, image := sh.avatar, ""
			if *a != nil {
				image = (*a).Image
			}
			asked = append(asked, CoverOf{Kind: sh.kind, ID: sh.id, Cover: Cover{Image: image}, Preset: AvatarPreset})
			put = append(put, func(r []Rendition) {
				if len(r) > 0 && *a == nil {
					*a = &Avatar{}
				}
				if *a != nil {
					(*a).Renditions = r
				}
			})
		}
		if s.gate != nil && gk.Entitled && sh.access != nil {
			sold, soldAt = append(sold, string(sh.id)), append(soldAt, sh.access)
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
	if len(sold) > 0 {
		keys := make([]access.Key, len(sold))
		for i, id := range sold {
			var err error
			keys[i], err = access.EntityKey(s.tenant, id)
			if err != nil {
				return err
			}
		}
		held, err := s.gate.Holds(ctx, s.viewer(), keys)
		unknown := errors.Is(err, access.ErrUnavailable)
		if err != nil && !unknown {
			return err
		}
		editors := s.contentEditors(ctx, sold)
		for i, at := range soldAt {
			*at = &EntityAccess{Key: keys[i], Member: held[i], Editor: editors[sold[i]], Unknown: unknown}
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
			if out[g.id] = owner; !owner && g.scope != "" && s.cfg.EntityKinds[g.kind].Owner == "user" {
				scoped = append(scoped, g)
			}
		}
		return rows.Err()
	})
	if err == nil && len(scoped) > 0 && s.scopes != nil && v.Member() {
		var held map[string][]string
		if held, err = s.heldScopes(ctx, v, ""); err == nil {
			for _, g := range scoped {
				out[g.id] = slices.Contains(held[g.scope], access.EntityContentWrite(g.kind))
			}
		}
	} else if err == nil {
		for _, g := range scoped {
			if out[g.id], err = gate.EntityEditor(ctx, s.as.auth, v, "", g.scope, access.EntityContentWrite(g.kind)); err != nil {
				break
			}
		}
	}
	if err != nil {
		s.log.WarnContext(ctx, "entities: content editor check failed; reading as a non-editor", "err", err)
	}
	return out
}
