package catalog

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// AuthError is a permission check the auth could not answer: 401 when the
// credential ended, else 503. It never grants.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "catalog: permission check failed: " + e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// staffRights are the actor's root permissions over content.
type staffRights struct{ write, view bool }

func (s *Store) staffRights(ctx context.Context, actor access.Actor) (staffRights, error) {
	if actor.Anonymous || actor.ID == "" {
		return staffRights{}, nil
	}
	var r staffRights
	var err error
	if r.write, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentWrite)); err != nil {
		return r, &AuthError{err}
	}
	if r.view, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentViewRestricted)); err != nil {
		return r, &AuthError{err}
	}
	return r, nil
}

// groupEditor reports, per owner group, whether actor edits its content: the
// owner of a user-owned group, or a holder of access.GroupContentWrite of its
// kind in its AuthKit scope.
func (s *Store) groupEditor(ctx context.Context, q Querier, actor access.Actor, groupIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if actor.Anonymous || actor.ID == "" || len(groupIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, kind, coalesce(owner_user_id, ''), coalesce(auth_scope, '') FROM `+s.t.groups+`
 WHERE tenant_id = $1 AND id = ANY($2) AND (owner_user_id IS NOT NULL OR auth_scope IS NOT NULL)`, s.tenant, groupIDs)
	if err != nil {
		return nil, err
	}
	type group struct{ kind, owner, scope string }
	found := map[string]group{}
	for rows.Next() {
		var id string
		var g group
		if err := rows.Scan(&id, &g.kind, &g.owner, &g.scope); err != nil {
			rows.Close()
			return nil, err
		}
		found[id] = g
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, g := range found {
		ok, err := gate.GroupEditor(ctx, s.guard.Auth, actor, g.owner, g.scope, access.GroupContentWrite(g.kind))
		if err != nil {
			return nil, &AuthError{err}
		}
		out[id] = ok
	}
	return out, nil
}

// readEditors is groupEditor for reads: a check that fails answers "not an
// editor", logged, so it never fails a read of what the rule shows anyway.
func (s *Store) readEditors(ctx context.Context, actor access.Actor, groupIDs []string) map[string]bool {
	editors, err := s.groupEditor(ctx, s.pool, actor, groupIDs)
	if err != nil {
		s.log.WarnContext(ctx, "catalog: owner-group editor check failed; reading as a non-editor", "groups", groupIDs, "err", err)
		return map[string]bool{}
	}
	return editors
}

// owned is what authorizing a write learned about one content item.
type owned struct {
	id, kind, state, ownerGroup string
	publishedAt, publishAt      bool
	everSold                    bool
}

// grants are the provider's answers a write needs, asked before its
// transaction: the provider may share the pool, so a transaction must never
// hold a connection and row locks while waiting on it. They last one write.
type grants struct {
	staff staffRights
	// editors are the owner groups the rows named when asked, and whether
	// the actor edits each.
	editors map[string]bool
}

// grantsFor asks the provider what writing ids needs, before any
// transaction: staff rights, else editorship of the owner groups the rows
// name now, read without locking. authorize rechecks them under the lock.
func (s *Store) grantsFor(ctx context.Context, actor access.Actor, ids []string) (grants, error) {
	var g grants
	var err error
	if g.staff, err = s.staffRights(ctx, actor); err != nil || g.staff.write {
		return g, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT owner_group_id FROM `+s.t.items+`
 WHERE tenant_id = $1 AND id = ANY($2) AND owner_group_id IS NOT NULL`, s.tenant, ids)
	if err != nil {
		return g, err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return g, err
	}
	g.editors, err = s.groupEditor(ctx, s.pool, actor, groups)
	return g, err
}

// authorize locks ids for the write and refuses unless actor edits each: a
// root:content:write holder, one of its creators or an editor of its owner
// group, by g. An owner group that changed since g was asked is not in it,
// so refuses. A missing id is ErrNotFound.
func (s *Store) authorize(ctx context.Context, q Querier, actor access.Actor, ids []string, g grants) (map[string]owned, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	rows, err := q.Query(ctx, `SELECT c.id, c.kind, c.state, coalesce(c.owner_group_id, ''), c.published_at IS NOT NULL, c.publish_at IS NOT NULL, c.ever_sold,
 EXISTS (SELECT 1 FROM `+s.t.creators+` cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = $3 AND $3 <> '')
 FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id = ANY($2) ORDER BY c.id FOR UPDATE`, s.tenant, ids, viewerID(actor))
	if err != nil {
		return nil, err
	}
	out := make(map[string]owned, len(ids))
	creator := map[string]bool{}
	for rows.Next() {
		var o owned
		var isCreator bool
		if err := rows.Scan(&o.id, &o.kind, &o.state, &o.ownerGroup, &o.publishedAt, &o.publishAt, &o.everSold, &isCreator); err != nil {
			rows.Close()
			return nil, err
		}
		out[o.id], creator[o.id] = o, isCreator
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, ErrNotFound
		}
	}
	if g.staff.write {
		return out, s.recordBeforeEdit(ctx, q, ids, out)
	}
	for id, o := range out {
		if !creator[id] && !g.editors[o.ownerGroup] {
			return nil, ErrForbidden
		}
	}
	return out, s.recordBeforeEdit(ctx, q, ids, out)
}

// authorizeCreate refuses unless actor may create content in ownerGroup: a
// root:content:write holder anywhere, an owner group's editor in it.
func (s *Store) authorizeCreate(ctx context.Context, q Querier, actor access.Actor, ownerGroups []string) error {
	rights, err := s.staffRights(ctx, actor)
	if err != nil || rights.write {
		return err
	}
	if !actor.Person() {
		return ErrForbidden
	}
	editors, err := s.groupEditor(ctx, q, actor, ownerGroups)
	if err != nil {
		return err
	}
	for _, g := range ownerGroups {
		if !editors[g] {
			return ErrForbidden
		}
	}
	if slices.Contains(ownerGroups, "") {
		return ErrForbidden
	}
	return nil
}

func viewerID(a access.Actor) string {
	return a.AccountID()
}

func isAuthErr(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}
