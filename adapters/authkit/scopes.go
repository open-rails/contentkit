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
	"slices"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit"
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
func Scopes(c Client) groups.Scopes { return scopes{c: c} }

// AppScopes isolates provider authorization groups even when two apps reuse a
// content group ID. Existing content references remain unchanged.
func AppScopes(c Client, scope contentkit.Scope) (groups.Scopes, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("contentkit/authkit: scope client is required")
	}
	if _, ok := c.(effectiveScopeClient); !ok {
		return nil, errors.New("contentkit/authkit: app scopes require effective Can")
	}
	return scopes{c: c, app: &scope}, nil
}

type scopes struct {
	c   Client
	app *contentkit.Scope
}

// ContentKitScope identifies the app binding of this provider; legacy Scopes
// returns empty IDs and cannot be used for a scoped app's authorization groups.
func (s scopes) ContentKitScope() (string, string) {
	if s.app == nil {
		return "", ""
	}
	return s.app.TenantID.String(), s.app.AppID.String()
}

type effectiveScopeClient interface {
	Can(context.Context, auth.Identity, iam.GroupRef, iam.Perm) (bool, error)
}

func (s scopes) providerID(id string) (string, error) {
	if s.app == nil {
		return id, nil
	}
	return GroupScopeID(*s.app, id)
}

// Provision creates each scope as a group of its kind's persona, its owner
// seeded with the persona's owner role; one that exists live is kept as it
// is. A scope retired before (soft-deleted) is purged and created anew: a
// restored group starts a new team with its owner.
func (s scopes) Provision(ctx context.Context, list []groups.Scope) error {
	for _, sc := range list {
		id, err := s.providerID(sc.ID)
		if err != nil {
			return err
		}
		persona, err := s.c.Persona(sc.Kind)
		if err != nil {
			return fmt.Errorf("authkit: persona of group kind %s: %w", sc.Kind, err)
		}
		g := iam.NewGroup{ID: id, Persona: persona}
		if s.app != nil {
			g.TenantID, err = iam.ParseTenantID(s.app.TenantID.String())
			if err != nil {
				return err
			}
		}
		if sc.Owner != "" {
			owner := iam.UserSubject(sc.Owner)
			g.Owner = &owner
		}
		_, err = s.c.CreateGroup(ctx, g)
		if errors.Is(err, iam.ErrGroupConflict) {
			ref := iam.GroupByID(id)
			old, gerr := s.c.Group(ctx, ref)
			if gerr != nil || old.DeletedAt == nil || old.Persona != persona || s.app != nil && old.TenantID != g.TenantID {
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
		id, err := s.providerID(id)
		if err != nil {
			return err
		}
		if s.app != nil {
			group, err := s.c.Group(ctx, iam.GroupByID(id))
			if errors.Is(err, iam.ErrGroupNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if group.TenantID.String() != s.app.TenantID.String() {
				return auth.ErrForbidden
			}
		}
		if err := s.c.DeleteGroup(ctx, iam.GroupByID(id)); err != nil && !errors.Is(err, iam.ErrGroupNotFound) {
			return fmt.Errorf("authkit: retire scope %s: %w", id, err)
		}
	}
	return nil
}

// Holding answers, per live group of the kinds' personas the user is a member
// of, which of its kind's access.GroupPermissions the user's effective
// permissions there cover: one memberships listing and one
// EffectivePermissions read.
func (s scopes) Holding(ctx context.Context, user string, kinds []string) (map[string][]string, error) {
	if s.app != nil {
		return nil, errors.New("contentkit/authkit: app scope lookups require verified local group IDs")
	}
	type named struct {
		name string
		perm iam.Perm
	}
	want := map[iam.Persona][]named{}
	for _, k := range kinds {
		persona, err := s.c.Persona(k)
		if err != nil {
			return nil, fmt.Errorf("authkit: persona of group kind %s: %w", k, err)
		}
		for _, name := range access.GroupPermissions(k) {
			p, err := s.c.Permission(name)
			if err != nil {
				return nil, fmt.Errorf("authkit: %w", err)
			}
			want[persona] = append(want[persona], named{name, p})
		}
	}
	var refs []iam.GroupRef
	asked := map[string][]named{}
	for m, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.Membership], error) {
		return s.c.ListMemberships(ctx, iam.UserSubject(user), p)
	}) {
		if err != nil {
			return nil, err
		}
		if ps, ok := want[m.Group.Persona]; ok && m.Group.DeletedAt == nil {
			refs, asked[m.Group.ID] = append(refs, iam.GroupByID(m.Group.ID)), ps
		}
	}
	out := map[string][]string{}
	if len(refs) == 0 {
		return out, nil
	}
	grants, err := s.c.EffectivePermissions(ctx, iam.UserIdentity(user), refs)
	if err != nil {
		return nil, err
	}
	for id, gs := range grants {
		for _, p := range asked[id] {
			if slices.ContainsFunc(gs, p.perm.Matches) {
				out[id] = append(out[id], p.name)
			}
		}
	}
	return out, nil
}

func (s scopes) HoldingScopes(ctx context.Context, user string, kinds, ids []string) (map[string][]string, error) {
	if s.app == nil {
		return s.Holding(ctx, user, kinds)
	}
	out := map[string][]string{}
	for _, id := range ids {
		providerID, err := s.providerID(id)
		if err != nil {
			return nil, err
		}
		group, err := s.c.Group(ctx, iam.GroupByID(providerID))
		if errors.Is(err, iam.ErrGroupNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if group.DeletedAt != nil || !slices.Contains(kinds, group.Persona.String()) || group.TenantID.String() != s.app.TenantID.String() {
			continue
		}
		for _, name := range access.GroupPermissions(group.Persona.String()) {
			perm, err := s.c.Permission(name)
			if err != nil {
				return nil, err
			}
			allowed, err := s.c.(effectiveScopeClient).Can(ctx, iam.UserIdentity(user), iam.GroupByID(providerID), perm)
			if err != nil {
				return nil, err
			}
			if allowed {
				out[id] = append(out[id], name)
			}
		}
	}
	return out, nil
}
