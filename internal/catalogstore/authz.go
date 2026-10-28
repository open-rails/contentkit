package catalogstore

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
)

// staffRights are the actor's root permissions over content.
type staffRights struct{ write, view bool }

func (s *Store) staffRights(ctx context.Context, actor access.Actor) (staffRights, error) {
	if actor.Anonymous || actor.ID == "" {
		return staffRights{}, nil
	}
	var r staffRights
	var err error
	if r.write, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentWrite)); err != nil {
		return r, &catalog.AuthError{Err: err}
	}
	if r.view, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentViewRestricted)); err != nil {
		return r, &catalog.AuthError{Err: err}
	}
	return r, nil
}

// groupEditor reports, per owner entity, whether actor edits its content: the
// owner of a user-owned entity, or a holder of access.EntityContentWrite of its
// kind in its AuthKit scope.
func (s *Store) entityEditor(ctx context.Context, q Querier, actor access.Actor, entityIDs []string) (map[string]bool, error) {
	return s.entityAllowed(ctx, q, actor, entityIDs, false)
}

// entityAllowed keeps narrow creation separate from broad channel editorship.
func (s *Store) entityAllowed(ctx context.Context, q Querier, actor access.Actor, entityIDs []string, creating bool) (map[string]bool, error) {
	out := map[string]bool{}
	if actor.Anonymous || actor.ID == "" || len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, kind, coalesce(owner_user_id, ''), coalesce(auth_scope, '') FROM `+s.t.entities+`
 WHERE tenant_id = $1 AND id = ANY($2) AND kind = 'channel' AND state = 'active' AND auth_scope IS NOT NULL`, s.tenant, entityIDs)
	if err != nil {
		return nil, err
	}
	type entity struct{ kind, owner, scope string }
	found := map[string]entity{}
	for rows.Next() {
		var id string
		var g entity
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
		if g.kind != "channel" || g.scope == "" {
			continue
		}
		permissions := []string{access.ChannelContentWrite}
		if creating {
			permissions = append(permissions, access.ChannelContentCreate)
		}
		ok, err := gate.ChannelEditor(ctx, s.guard.Auth, actor, g.scope, permissions...)
		if err != nil {
			return nil, &catalog.AuthError{Err: err}
		}
		out[id] = ok
	}
	return out, nil
}

// readEditors is groupEditor for reads: a check that fails answers "not an
// editor", logged, so it never fails a read of what the rule shows anyway.
func (s *Store) readEditors(ctx context.Context, actor access.Actor, entityIDs []string) map[string]bool {
	editors, err := s.entityEditor(ctx, s.pool, actor, entityIDs)
	if err != nil {
		s.log.WarnContext(ctx, "catalog: owner-group editor check failed; reading as a non-editor", "groups", entityIDs, "err", err)
		return map[string]bool{}
	}
	return editors
}

// owned is what authorizing a write learned about one content item.
type owned struct {
	id, kind, state, ownerEntity string
	publishedAt, publishAt       bool
}

// grants are the provider's answers a write needs, asked before its
// transaction: the provider may share the pool, so a transaction must never
// hold a connection and row locks while waiting on it. They last one write.
type grants struct {
	staff staffRights
	own   ownAccess
	// editors are the owner entities the rows named when asked, and whether
	// the actor edits each.
	editors map[string]bool
}

// grantsFor asks the provider what writing ids needs, before any
// transaction: staff rights, else editorship of the owner entities the rows
// name now, read without locking. authorize rechecks them under the lock.
func (s *Store) grantsFor(ctx context.Context, actor access.Actor, ids []string) (grants, error) {
	var g grants
	var err error
	if g.staff, err = s.staffRights(ctx, actor); err != nil || g.staff.write {
		return g, err
	}
	if g.own, err = s.ownAccess(ctx, actor, true); err != nil {
		return g, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT owner_entity_id FROM `+s.t.items+`
 WHERE tenant_id = $1 AND id = ANY($2) AND owner_entity_id IS NOT NULL`, s.tenant, ids)
	if err != nil {
		return g, err
	}
	entities, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return g, err
	}
	g.editors, err = s.entityEditor(ctx, s.pool, actor, entities)
	return g, err
}

// authorize locks ids for the write and refuses unless actor edits each: a
// root:content:write holder, its authenticated author or an editor of its owner
// entity, by g. An owner entity that changed since g was asked is not in it,
// so refuses. A missing id is ErrNotFound.
func (s *Store) authorize(ctx context.Context, q Querier, actor access.Actor, ids []string, g grants) (map[string]owned, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	rows, err := q.Query(ctx, `SELECT c.id, c.kind, c.state, coalesce(c.owner_entity_id, ''), c.published_at IS NOT NULL, c.publish_at IS NOT NULL,
 (coalesce(c.created_by, '') = $3 AND $3 <> '')
 FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id = ANY($2) ORDER BY c.id FOR UPDATE`, s.tenant, ids, viewerID(actor))
	if err != nil {
		return nil, err
	}
	out := make(map[string]owned, len(ids))
	creator := map[string]bool{}
	for rows.Next() {
		var o owned
		var isCreator bool
		if err := rows.Scan(&o.id, &o.kind, &o.state, &o.ownerEntity, &o.publishedAt, &o.publishAt, &isCreator); err != nil {
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
		if !(creator[id] && g.own.allows(o.ownerEntity)) && !g.editors[o.ownerEntity] {
			return nil, ErrForbidden
		}
	}
	return out, s.recordBeforeEdit(ctx, q, ids, out)
}

// authorizeCreate admits staff anywhere, personal creation outside channels,
// or a channel's owner/contributor/editor inside that specific channel.
func (s *Store) authorizeCreate(ctx context.Context, q Querier, actor access.Actor, ownerEntities []string) error {
	rights, err := s.staffRights(ctx, actor)
	if err != nil || rights.write {
		return err
	}
	if !actor.Member() {
		return ErrForbidden
	}
	if slices.Contains(ownerEntities, "") {
		allowed, err := gate.Can(ctx, s.guard.Auth, actor, string(access.ContentCreate))
		if err != nil {
			return &catalog.AuthError{Err: err}
		}
		if !allowed {
			return ErrForbidden
		}
	}
	allowed, err := s.entityAllowed(ctx, q, actor, ownerEntities, true)
	if err != nil {
		return err
	}
	for _, entity := range ownerEntities {
		if entity != "" && !allowed[entity] {
			return ErrForbidden
		}
	}
	return nil
}

func viewerID(a access.Actor) string {
	return a.AccountID()
}

// ownAccess is evaluated once per request from current provider grants. The
// author ID is still required at each content row; it is not itself a grant.
type ownAccess struct {
	app      bool
	entities []string
}

func (a ownAccess) allows(channel string) bool {
	if channel == "" {
		return a.app
	}
	return slices.Contains(a.entities, channel)
}

func (s *Store) ownAccess(ctx context.Context, actor access.Actor, editing bool) (ownAccess, error) {
	out := ownAccess{entities: []string{}}
	if !actor.Member() {
		return out, nil
	}
	var err error
	out.app, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentEditOwn))
	if err != nil {
		return out, &catalog.AuthError{Err: err}
	}
	if !editing && !out.app {
		out.app, err = gate.Can(ctx, s.guard.Auth, actor, string(access.ContentCreate))
		if err != nil {
			return out, &catalog.AuthError{Err: err}
		}
	}
	if source, ok := s.entities.(interface {
		ContentAuthorEntities(context.Context, auth.Authenticator, access.Actor, bool) ([]entities.EntityID, error)
	}); ok {
		ids, err := source.ContentAuthorEntities(ctx, s.guard.Auth, actor, editing)
		if err != nil {
			return out, &catalog.AuthError{Err: err}
		}
		for _, id := range ids {
			out.entities = append(out.entities, string(id))
		}
	}
	return out, nil
}
