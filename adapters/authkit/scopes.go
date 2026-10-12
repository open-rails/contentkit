// Package authkit keeps ContentKit's group scopes in AuthKit: the
// groups.Scopes an Engine takes as Deps.Scopes. A group kind with
// auth_scope names an AuthKit persona of the same name; each of its groups
// is guarded by an AuthKit group keyed by the ContentKit group's id, whose
// holders of {kind}:settings:manage and {kind}:media:write edit it
// (access.GroupPermissions).
package authkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
)

// Client is what Scopes needs of AuthKit: *authkit.Client.
type Client interface {
	Persona(name string) (iam.Persona, error)
	Group(ctx context.Context, ref iam.GroupRef) (iam.Group, error)
	CreateGroup(ctx context.Context, g iam.NewGroup, opts ...authkit.Option) (iam.Group, error)
	DeleteGroup(ctx context.Context, ref iam.GroupRef, opts ...authkit.Option) error
	PurgeGroup(ctx context.Context, ref iam.GroupRef, opts ...authkit.Option) error
	Permission(text string) (iam.Perm, error)
	ListMemberships(ctx context.Context, s iam.Subject, p iam.PageRequest) (iam.ListPage[iam.Membership], error)
	EffectivePermissions(ctx context.Context, who auth.Identity, refs []iam.GroupRef) (map[string][]iam.Perm, error)
}

var _ Client = (*authkit.Client)(nil)

// Scopes is groups.Scopes over c.
func Scopes(c Client) groups.Scopes { return scopes{c} }

type scopes struct{ c Client }

// Provision creates each scope as a group of its kind's persona, its owner
// seeded with the persona's owner role; one that exists live is kept as it
// is. A scope retired before (soft-deleted) is purged and created anew: a
// restored group starts a new team with its owner.
func (s scopes) Provision(ctx context.Context, list []groups.Scope) error {
	for _, sc := range list {
		persona, err := s.c.Persona(sc.Kind)
		if err != nil {
			return fmt.Errorf("authkit: persona of group kind %s: %w", sc.Kind, err)
		}
		g := iam.NewGroup{ID: sc.ID, Persona: persona}
		if sc.Owner != "" {
			owner := iam.UserSubject(sc.Owner)
			g.Owner = &owner
		}
		_, err = s.c.CreateGroup(ctx, g)
		if errors.Is(err, iam.ErrGroupConflict) {
			ref := iam.GroupByID(sc.ID)
			old, gerr := s.c.Group(ctx, ref)
			if gerr != nil || old.DeletedAt == nil || old.Persona != persona {
				return errors.Join(fmt.Errorf("authkit: scope %s is another group", sc.ID), err, gerr)
			}
			if err = s.c.PurgeGroup(ctx, ref); err == nil {
				_, err = s.c.CreateGroup(ctx, g)
			}
		}
		if err != nil {
			return fmt.Errorf("authkit: provision scope %s: %w", sc.ID, err)
		}
	}
	return nil
}

// Retire soft-deletes each scope: it stops resolving and granting at once.
func (s scopes) Retire(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := s.c.DeleteGroup(ctx, iam.GroupByID(id)); err != nil && !errors.Is(err, iam.ErrGroupNotFound) {
			return fmt.Errorf("authkit: retire scope %s: %w", id, err)
		}
	}
	return nil
}

// Holding is the user's live groups of the kinds' personas whose effective
// permissions cover the kind's access.GroupSettingsManage: one memberships
// listing and one EffectivePermissions read.
func (s scopes) Holding(ctx context.Context, user string, kinds []string) ([]string, error) {
	want := map[iam.Persona]iam.Perm{}
	for _, k := range kinds {
		persona, err := s.c.Persona(k)
		if err != nil {
			return nil, fmt.Errorf("authkit: persona of group kind %s: %w", k, err)
		}
		if want[persona], err = s.c.Permission(access.GroupSettingsManage(k)); err != nil {
			return nil, fmt.Errorf("authkit: %w", err)
		}
	}
	var refs []iam.GroupRef
	perm := map[string]iam.Perm{}
	for m, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.Membership], error) {
		return s.c.ListMemberships(ctx, iam.UserSubject(user), p)
	}) {
		if err != nil {
			return nil, err
		}
		if p, ok := want[m.Group.Persona]; ok && m.Group.DeletedAt == nil {
			refs, perm[m.Group.ID] = append(refs, iam.GroupByID(m.Group.ID)), p
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	grants, err := s.c.EffectivePermissions(ctx, iam.UserIdentity(user), refs)
	if err != nil {
		return nil, err
	}
	var out []string
	for id, gs := range grants {
		for _, g := range gs {
			if perm[id].Matches(g) {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}
