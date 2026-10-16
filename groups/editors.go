package groups

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// writer is the actor a store writes as (As).
type writer struct {
	auth  auth.Authenticator
	actor access.Actor
	once  sync.Once
	staff bool
	err   error
}

// As returns the store acting as actor, judged by a. Its reads show actor's
// standing with sellable groups. Its writes: staff holding
// access.EntitiesWrite in a's root scope (and the engine's own client) write
// anything; anyone else writes only user-owned groups, under the creator
// rules of CreateGroups and UpdateGroups. Without As a store reads as an
// anonymous viewer and writes as the host, unchecked.
func (s *Store) As(a auth.Authenticator, actor access.Actor) *Store {
	c := *s
	c.as = &writer{auth: a, actor: actor}
	return &c
}

// viewer is the actor reads show standings for.
func (s *Store) viewer() access.Actor {
	if s.as == nil {
		return access.Actor{Anonymous: true}
	}
	return s.as.actor
}

// staff reports a write as the host or as staff.
func (s *Store) staff(ctx context.Context) (bool, error) {
	w := s.as
	if w == nil {
		return true, nil
	}
	w.once.Do(func() { w.staff, w.err = gate.Can(ctx, w.auth, w.actor, string(access.EntitiesWrite)) })
	return w.staff, w.err
}

// edits reports whether the non-staff writer edits g.
func (w *writer) edits(ctx context.Context, s *Store, g Group) (bool, error) {
	if g.Internal || s.cfg.GroupKinds[g.Kind].Owner != "user" {
		return false, nil
	}
	return gate.GroupEditor(ctx, w.auth, w.actor, g.OwnerUserID, g.AuthScope, access.GroupSettingsManage(g.Kind))
}

// mayEdit refuses, with ErrForbidden, a group the writer does not edit.
func (s *Store) mayEdit(ctx context.Context, gs []Group) error {
	staff, err := s.staff(ctx)
	if err != nil || staff {
		return err
	}
	for _, g := range gs {
		ok, err := s.as.edits(ctx, s, g)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: group %s", ErrForbidden, g.ID)
		}
	}
	return nil
}

// creator checks a non-staff writer's new groups: a person's own, of
// user-owned kinds, with neither an id nor an AuthKit scope chosen. It
// answers the person's id, "" for staff.
func (s *Store) creator(ctx context.Context, inputs []GroupInput) (string, error) {
	staff, err := s.staff(ctx)
	if err != nil || staff {
		return "", err
	}
	a := s.as.actor
	if !a.Member() {
		return "", fmt.Errorf("%w: only a signed-in person creates groups", ErrForbidden)
	}
	for _, in := range inputs {
		kind := strings.TrimSpace(in.Kind)
		switch gk, ok := s.cfg.GroupKinds[kind]; {
		case !ok:
			return "", fmt.Errorf("%w: group kind %q is not configured", ErrInvalid, kind)
		case gk.Owner != "user":
			return "", fmt.Errorf("%w: %s groups are the site's", ErrForbidden, kind)
		case in.ID != "" || in.AuthScope != "" || in.Internal:
			return "", fmt.Errorf("%w: only staff choose a group's id, auth scope or internal classification", ErrForbidden)
		case in.OwnerUserID != "" && in.OwnerUserID != a.AccountID():
			return "", fmt.Errorf("%w: a person creates only groups of their own", ErrForbidden)
		}
	}
	return a.AccountID(), nil
}

// mayLink refuses a non-staff writer's parent or scope it does not edit.
func (s *Store) mayLink(ctx context.Context, q Querier, rows []groupRow) error {
	if staff, err := s.staff(ctx); err != nil || staff {
		return err
	}
	var links []string
	for _, r := range rows {
		if r.ParentID != nil {
			links = append(links, *r.ParentID)
		}
		if r.ScopeID != "" {
			links = append(links, r.ScopeID)
		}
	}
	if len(links) == 0 {
		return nil
	}
	found, err := s.byIDs(ctx, q, links)
	if err != nil {
		return err
	}
	gs := make([]Group, 0, len(found))
	for _, g := range found {
		gs = append(gs, g)
	}
	return s.mayEdit(ctx, gs)
}

// ownerKey is one person's groups of one capped kind.
type ownerKey struct{ kind, owner string }

// capped lists the capped (kind, owner) pairs rows activate, for a
// non-staff writer.
func (s *Store) capped(ctx context.Context, rows []groupRow) ([]ownerKey, error) {
	if staff, err := s.staff(ctx); err != nil || staff {
		return nil, err
	}
	var out []ownerKey
	for _, r := range rows {
		if r.OwnerUserID == nil || s.cfg.GroupKinds[r.Kind].MaxPerOwner == 0 {
			continue
		}
		k := ownerKey{r.Kind, *r.OwnerUserID}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b ownerKey) int { return strings.Compare(a.kind+"\x00"+a.owner, b.kind+"\x00"+b.owner) })
	return out, nil
}

// lockOwners serializes, for the transaction, the writes that may take a
// capped owner's last group: checkCaps then counts what committed before.
func (s *Store) lockOwners(ctx context.Context, q Querier, rows []groupRow) error {
	keys, err := s.capped(ctx, rows)
	for _, k := range keys {
		if err != nil {
			break
		}
		_, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "contentkit:group-owner:"+s.tenant+":"+k.kind+":"+k.owner)
	}
	return err
}

// checkCaps refuses, with ErrLimit, an owner past a kind's MaxPerOwner
// active groups, the transaction's own included.
func (s *Store) checkCaps(ctx context.Context, q Querier, rows []groupRow) error {
	keys, err := s.capped(ctx, rows)
	if err != nil {
		return err
	}
	for _, k := range keys {
		var n int
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1 AND kind = $2 AND owner_user_id = $3 AND state = 'active'`,
			s.table("content_groups")), s.tenant, k.kind, k.owner).Scan(&n); err != nil {
			return err
		}
		if limit := s.cfg.GroupKinds[k.kind].MaxPerOwner; n > limit {
			return fmt.Errorf("%w: at most %d %s groups each", ErrLimit, limit, k.kind)
		}
	}
	return nil
}
