package groups

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/offers"
)

// AvatarPreset is the media preset avatars render in (media.AvatarPreset).
const AvatarPreset = "avatar"

// Sales answers sellable groups' sales, every group of a page in one call:
// offers.Service.
type Sales interface {
	GroupAccess(ctx context.Context, viewer access.Actor, groups []string) ([]offers.GroupAccess, error)
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
		for i, at := range soldAt {
			*at = &got[i]
		}
	}
	return nil
}
