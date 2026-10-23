package entities

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/offers"
)

// AvatarPreset is the media preset avatars render in (media.AvatarPreset).
const AvatarPreset = "avatar"

// Sales answers sellable entities' sales, every entity of a page in one call,
// the entities a viewer holds the key of and, for their offer editors, entities'
// current offers, and re-syncs the products of renamed entities in the writing
// transaction: offers.Service.
type Sales interface {
	EntityAccess(ctx context.Context, viewer access.Actor, entities []string) ([]offers.EntityAccess, error)
	Joined(ctx context.Context, viewer access.Actor) ([]string, error)
	Current(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []offers.Target) ([]*offers.Offer, error)
	Resync(ctx context.Context, tx pgx.Tx, targets []offers.Target) error
}

// currentOffer is g's current offer when the store's viewer edits its offers;
// nil otherwise, or when the check fails.
func (s *Store) currentOffer(ctx context.Context, g Entity) *offers.Offer {
	if s.sales == nil || s.as == nil || !s.cfg.EntityKinds[g.Kind].Sellable {
		return nil
	}
	got, err := s.sales.Current(ctx, s.as.auth, s.as.actor, []offers.Target{{Kind: offers.EntityTarget, ID: string(g.ID)}})
	if err != nil {
		s.log.WarnContext(ctx, "entities: offer editor check failed; reading without the offer", "group", g.ID, "err", err)
		return nil
	}
	return got[0]
}

// resyncSold re-syncs the offers of the sellable entities among gs, whose
// products are named after them.
func (s *Store) resyncSold(ctx context.Context, q Querier, gs []Ref) error {
	tx, ok := q.(pgx.Tx)
	if s.sales == nil || !ok {
		return nil
	}
	var targets []offers.Target
	for _, g := range gs {
		if s.cfg.EntityKinds[g.Kind].Sellable {
			targets = append(targets, offers.Target{Kind: offers.EntityTarget, ID: string(g.ID)})
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return s.sales.Resync(ctx, tx, targets)
}

// shown is one entity a page shows; fill completes it in place.
type shown struct {
	kind   string
	id     EntityID
	cover  *Cover
	avatar **Avatar
	access **offers.EntityAccess
}

func cardShown(g *EntityCard) shown {
	out := shown{kind: g.Kind, id: g.ID, cover: g.Cover, avatar: &g.Avatar}
	if !g.Internal {
		out.access = &g.Access
	}
	return out
}

// fill resolves the covers (contents' in language) and the user-owned
// entities' avatars of a page in one call, and the sellable entities' sales, for
// the store's viewer, in one more.
func (s *Store) fill(ctx context.Context, list []shown, language string) error {
	var asked []CoverOf
	var put []func([]Rendition)
	var sold []string
	var soldAt []**offers.EntityAccess
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
		if s.sales != nil && gk.Sellable && sh.access != nil {
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
		got, err := s.sales.EntityAccess(ctx, s.viewer(), sold)
		if err != nil {
			return err
		}
		editors := s.contentEditors(ctx, sold)
		for i, at := range soldAt {
			if editors[sold[i]] {
				got[i].Editor, got[i].Offer = true, nil
			}
			*at = &got[i]
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
