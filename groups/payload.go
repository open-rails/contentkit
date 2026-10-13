package groups

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

// Sales answers sellable groups' sales, every group of a page in one call,
// the groups a viewer holds the key of and, for their offer editors, groups'
// current offers, and re-syncs the products of renamed groups in the writing
// transaction: offers.Service.
type Sales interface {
	GroupAccess(ctx context.Context, viewer access.Actor, groups []string) ([]offers.GroupAccess, error)
	Joined(ctx context.Context, viewer access.Actor) ([]string, error)
	Current(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []offers.Target) ([]*offers.Offer, error)
	Resync(ctx context.Context, tx pgx.Tx, targets []offers.Target) error
}

// currentOffer is g's current offer when the store's viewer edits its offers;
// nil otherwise, or when the check fails.
func (s *Store) currentOffer(ctx context.Context, g Group) *offers.Offer {
	if s.sales == nil || s.as == nil || !s.cfg.GroupKinds[g.Kind].Sellable {
		return nil
	}
	got, err := s.sales.Current(ctx, s.as.auth, s.as.actor, []offers.Target{{Kind: offers.GroupTarget, ID: string(g.ID)}})
	if err != nil {
		s.log.WarnContext(ctx, "groups: offer editor check failed; reading without the offer", "group", g.ID, "err", err)
		return nil
	}
	return got[0]
}

// resyncSold re-syncs the offers of the sellable groups among gs, whose
// products are named after them.
func (s *Store) resyncSold(ctx context.Context, q Querier, gs []Ref) error {
	tx, ok := q.(pgx.Tx)
	if s.sales == nil || !ok {
		return nil
	}
	var targets []offers.Target
	for _, g := range gs {
		if s.cfg.GroupKinds[g.Kind].Sellable {
			targets = append(targets, offers.Target{Kind: offers.GroupTarget, ID: string(g.ID)})
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return s.sales.Resync(ctx, tx, targets)
}

// shown is one group a page shows; fill completes it in place.
type shown struct {
	kind   string
	id     GroupID
	cover  *Cover
	avatar **Avatar
	access **offers.GroupAccess
}

func cardShown(g *GroupCard) shown {
	return shown{kind: g.Kind, id: g.ID, cover: g.Cover, avatar: &g.Avatar, access: &g.Access}
}

// fill resolves the covers and the user-owned groups' avatars of a page in
// one call, and the sellable groups' sales, for the store's viewer, in one
// more.
func (s *Store) fill(ctx context.Context, list []shown) error {
	var asked []CoverOf
	var put []func([]Rendition)
	var sold []string
	var soldAt []**offers.GroupAccess
	for _, sh := range list {
		gk := s.cfg.GroupKinds[sh.kind]
		if s.covers != nil && sh.cover != nil {
			c := sh.cover
			asked, put = append(asked, CoverOf{Kind: sh.kind, ID: sh.id, Cover: *c}), append(put, func(r []Rendition) { c.Renditions = r })
		}
		if s.covers != nil && gk.Owner == "user" && sh.avatar != nil {
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
		got, err := s.sales.GroupAccess(ctx, s.viewer(), sold)
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

// contentEditors reports, per group, whether the store's viewer edits its
// content: its owner, or a holder of {kind}:content:write in its AuthKit
// scope, read in one Scopes.Holding call for the page. A failed check reads
// as not an editor.
func (s *Store) contentEditors(ctx context.Context, ids []string) map[string]bool {
	out := map[string]bool{}
	v := s.viewer()
	if !v.Person() || len(ids) == 0 {
		return out
	}
	type group struct{ id, kind, scope string }
	var scoped []group
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, kind, coalesce(owner_user_id, '') = $3, coalesce(auth_scope, '') FROM %s
 WHERE tenant_id = $1 AND id = ANY($2::text[]) AND state = 'active'`, s.table("content_groups")), s.tenant, ids, v.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g group
			var owner bool
			if err := rows.Scan(&g.id, &g.kind, &owner, &g.scope); err != nil {
				return err
			}
			if out[g.id] = owner; !owner && g.scope != "" && s.cfg.GroupKinds[g.kind].Owner == "user" {
				scoped = append(scoped, g)
			}
		}
		return rows.Err()
	})
	if err == nil && len(scoped) > 0 && s.scopes != nil {
		var held map[string][]string
		if held, err = s.heldScopes(ctx, v.ID, ""); err == nil {
			for _, g := range scoped {
				out[g.id] = slices.Contains(held[g.scope], access.GroupContentWrite(g.kind))
			}
		}
	} else if err == nil {
		for _, g := range scoped {
			if out[g.id], err = gate.GroupEditor(ctx, s.as.auth, v, "", g.scope, access.GroupContentWrite(g.kind)); err != nil {
				break
			}
		}
	}
	if err != nil {
		s.log.WarnContext(ctx, "groups: content editor check failed; reading as a non-editor", "err", err)
	}
	return out
}
