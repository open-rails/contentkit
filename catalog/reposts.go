package catalog

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Repost authority is read before opening a transaction: AuthKit may share
// our pool. The transaction then revalidates the exact destination identity.
type repostGrant struct {
	revision      int64
	scope         string
	create, write bool
}

func (s *Store) repostGrant(ctx context.Context, actor access.Actor, id string) (repostGrant, error) {
	var out repostGrant
	if s.cfg.Channels == nil {
		return out, ErrNotFound
	}
	err := s.pool.QueryRow(ctx, "SELECT revision,auth_scope FROM "+s.t.entities+" WHERE tenant_id=$1 AND id=$2 AND kind='channel' AND state='active' AND auth_scope IS NOT NULL", s.tenant, id).Scan(&out.revision, &out.scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rights, err := s.staffRights(ctx, actor)
	if err != nil {
		return out, err
	}
	if rights.write {
		out.create, out.write = true, true
		return out, nil
	}
	writers, err := s.entityEditor(ctx, s.pool, actor, []string{id})
	if err != nil {
		return out, err
	}
	out.write = writers[id]
	if out.write {
		out.create = true
		return out, nil
	}
	creators, err := s.entityAllowed(ctx, s.pool, actor, []string{id}, true)
	out.create = creators[id]
	return out, err
}

func (s *Store) lockRepostDestination(ctx context.Context, tx pgx.Tx, id string, g repostGrant) error {
	var revision int64
	var scope string
	err := tx.QueryRow(ctx, "SELECT revision,auth_scope FROM "+s.t.entities+" WHERE tenant_id=$1 AND id=$2 AND kind='channel' AND state='active' AND auth_scope IS NOT NULL FOR SHARE", s.tenant, id).Scan(&revision, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision != g.revision || scope != g.scope {
		return errStale
	}
	return nil
}

// PutRepost adds one destination reference to a published original. It never
// changes original ownership, access, versions, media or interaction counters.
func (s *Store) PutRepost(ctx context.Context, actor access.Actor, destination, contentID, host string) (channels.Repost, error) {
	out := channels.Repost{ChannelID: destination}
	for _, id := range []string{destination, contentID} {
		if err := contentref.ValidateID(id); err != nil {
			return out, invalid("repost identifier: %v", err)
		}
	}
	grant, err := s.repostGrant(ctx, actor, destination)
	if err != nil {
		return out, err
	}
	if !grant.create {
		return out, ErrForbidden
	}
	var revision int64
	var owner, state string
	err = s.pool.QueryRow(ctx, "SELECT revision,coalesce(owner_entity_id,''),state FROM "+s.t.items+" WHERE tenant_id=$1 AND id=$2", s.tenant, contentID).Scan(&revision, &owner, &state)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (state != StatePublished || owner == "") {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if owner == destination {
		return out, invalid("the primary channel already contains this original")
	}
	viewer, err := s.Viewer(ctx, ViewerOptions{Actor: actor, Host: host, Mode: access.Hide})
	if err != nil {
		return out, err
	}
	views, err := viewer.decide(ctx, []string{contentID}, false)
	if err != nil {
		return out, err
	}
	allowed := []string{}
	if view := views[contentID]; view != nil {
		for _, version := range view.versions {
			if viewer.live(version) && !version.deleted && !version.contentDeleted && view.decisions[version.versionID].Allowed {
				allowed = append(allowed, version.versionID)
			}
		}
	}
	if len(allowed) == 0 {
		return out, ErrNotFound
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var current int64
		var kind, currentOwner, currentState string
		err := tx.QueryRow(ctx, "SELECT revision,kind,coalesce(owner_entity_id,''),state FROM "+s.t.items+" WHERE tenant_id=$1 AND id=$2 FOR SHARE", s.tenant, contentID).Scan(&current, &kind, &currentOwner, &currentState)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != revision || currentOwner != owner || currentState != StatePublished {
			return errStale
		}
		if err := s.lockRepostDestination(ctx, tx, destination, grant); err != nil {
			return err
		}
		clause, args := viewer.Listed("f")
		args["rp_tenant"], args["rp_content"], args["rp_versions"] = s.tenant, contentID, allowed
		var readable bool
		err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+s.t.facets+" f WHERE f.tenant_id=@rp_tenant AND f.content_id=@rp_content AND f.version_id=ANY(@rp_versions::text[]) AND NOT f.deleted AND NOT f.unplayable AND f.live_from<=@cv_now::timestamptz AND ("+clause+"))", args).Scan(&readable)
		if err != nil {
			return err
		}
		if !readable {
			return ErrNotFound
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+s.qs+`.content_channel_reposts(tenant_id,channel_id,content_id,actor_id) VALUES($1,$2,$3,nullif($4,'')) ON CONFLICT DO NOTHING`, s.tenant, destination, contentID, viewerID(actor))
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(actor_id,''),created_at FROM `+s.qs+`.content_channel_reposts WHERE tenant_id=$1 AND channel_id=$2 AND content_id=$3`, s.tenant, destination, contentID).Scan(&out.ActorID, &out.CreatedAt); err != nil {
			return err
		}
		out.CanRemove = grant.write || grant.create && out.ActorID != "" && out.ActorID == viewerID(actor)
		if tag.RowsAffected() > 0 {
			return s.emit(ctx, tx, Event{Type: "content.reposted", Kind: kind, ContentID: contentID, ChannelID: destination, ActorID: viewerID(actor)})
		}
		return nil
	})
	return out, err
}

func (s *Store) DeleteRepost(ctx context.Context, actor access.Actor, destination, contentID string) error {
	for _, id := range []string{destination, contentID} {
		if err := contentref.ValidateID(id); err != nil {
			return invalid("repost identifier: %v", err)
		}
	}
	grant, err := s.repostGrant(ctx, actor, destination)
	if err != nil {
		return err
	}
	if !grant.create {
		return ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, "SELECT kind FROM "+s.t.items+" WHERE tenant_id=$1 AND id=$2 FOR SHARE", s.tenant, contentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.lockRepostDestination(ctx, tx, destination, grant); err != nil {
			return err
		}
		var creator string
		err = tx.QueryRow(ctx, `SELECT coalesce(actor_id,'') FROM `+s.qs+`.content_channel_reposts WHERE tenant_id=$1 AND channel_id=$2 AND content_id=$3 FOR UPDATE`, s.tenant, destination, contentID).Scan(&creator)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !grant.write && (creator == "" || creator != viewerID(actor)) {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+s.qs+`.content_channel_reposts WHERE tenant_id=$1 AND channel_id=$2 AND content_id=$3`, s.tenant, destination, contentID); err != nil {
			return err
		}
		return s.emit(ctx, tx, Event{Type: "content.repost_removed", Kind: kind, ContentID: contentID, ChannelID: destination, ActorID: viewerID(actor)})
	})
}

func (s *Store) handlePutRepost(w http.ResponseWriter, r *http.Request) {
	out, err := s.PutRepost(r.Context(), gate.Actor(r.Context()), r.PathValue("id"), r.PathValue("content_id"), r.Host)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Store) handleDeleteRepost(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteRepost(r.Context(), gate.Actor(r.Context()), r.PathValue("id"), r.PathValue("content_id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 204, nil)
}

var repostRoutes = []httpapi.Route[*Store]{
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/channels/{id}/reposts/{content_id}", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ContentWrite), Doc: "Idempotently reposts a published original into a channel the caller may post to; original ownership and access remain unchanged.", Responses: []httpapi.Reply{httpapi.OK(channels.Repost{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound, httpapi.CodeConflict}}, Serve: httpapi.H((*Store).handlePutRepost)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/channels/{id}/reposts/{content_id}", Resource: "channels", Auth: httpapi.Editor, Perm: string(access.ContentWrite), Doc: "Removes only the destination repost reference. Requires destination editorship or the original reposter's current posting permission.", Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}}, Serve: httpapi.H((*Store).handleDeleteRepost)},
}
