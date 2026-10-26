package catalog

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"net/http"
)

// PutRepost adds one destination reference to published original content.
// It never changes the original's owner, access, versions, media or counters.
func (s *Store) PutRepost(ctx context.Context, actor access.Actor, destination, contentID, host string) (channels.Repost, error) {
	out := channels.Repost{ChannelID: destination}
	for _, id := range []string{destination, contentID} {
		if err := contentref.ValidateID(id); err != nil {
			return out, invalid("repost identifier: %v", err)
		}
	}
	if s.cfg.Channels == nil {
		return out, ErrNotFound
	}
	viewer, err := s.Viewer(ctx, ViewerOptions{Actor: actor, Host: host, Mode: access.Hide})
	if err != nil {
		return out, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind, owner, state string
		if err := tx.QueryRow(ctx, `SELECT kind,coalesce(owner_entity_id,''),state FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2 FOR SHARE`, s.tenant, contentID).Scan(&kind, &owner, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != StatePublished || owner == "" {
			return ErrNotFound
		}
		if owner == destination {
			return invalid("the primary channel already contains this original")
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT state='active' AND auth_scope IS NOT NULL FROM `+s.t.entities+` WHERE tenant_id=$1 AND id=$2 AND kind='channel' FOR SHARE`, s.tenant, destination).Scan(&active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !active {
			return ErrNotFound
		}
		allowed, err := s.canRepost(ctx, tx, actor, destination, false, "")
		if err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
		clause, args := viewer.Listed("f")
		args["repost_content"] = contentID
		var readable bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.facets+` f WHERE f.content_id=@repost_content AND NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL AND f.live_from<=now() AND (`+clause+`))`, args).Scan(&readable); err != nil {
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
		out.CanRemove, err = s.canRepost(ctx, tx, actor, destination, true, out.ActorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return s.emit(ctx, tx, Event{Type: "content.reposted", Kind: kind, ContentID: contentID, ChannelID: destination, ActorID: viewerID(actor)})
		}
		return nil
	})
	return out, err
}

func (s *Store) canRepost(ctx context.Context, q Querier, actor access.Actor, channelID string, remove bool, createdBy string) (bool, error) {
	rights, err := s.staffRights(ctx, actor)
	if err != nil {
		return false, err
	}
	if rights.write {
		return true, nil
	}
	if remove {
		writers, err := s.entityEditor(ctx, q, actor, []string{channelID})
		if err != nil {
			return false, err
		}
		if writers[channelID] {
			return true, nil
		}
		if viewerID(actor) == "" || viewerID(actor) != createdBy {
			return false, nil
		}
	}
	allowed, err := s.entityAllowed(ctx, q, actor, []string{channelID}, true)
	return allowed[channelID], err
}

func (s *Store) DeleteRepost(ctx context.Context, actor access.Actor, destination, contentID string) error {
	for _, id := range []string{destination, contentID} {
		if err := contentref.ValidateID(id); err != nil {
			return invalid("repost identifier: %v", err)
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var actorID, kind string
		err := tx.QueryRow(ctx, `SELECT coalesce(r.actor_id,''),c.kind FROM `+s.qs+`.content_channel_reposts r JOIN `+s.t.items+` c ON c.tenant_id=r.tenant_id AND c.id=r.content_id WHERE r.tenant_id=$1 AND r.channel_id=$2 AND r.content_id=$3 FOR UPDATE OF r`, s.tenant, destination, contentID).Scan(&actorID, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			allowed, e := s.canRepost(ctx, tx, actor, destination, false, "")
			if e != nil {
				return e
			}
			if !allowed {
				return ErrForbidden
			}
			return nil
		}
		if err != nil {
			return err
		}
		allowed, err := s.canRepost(ctx, tx, actor, destination, true, actorID)
		if err != nil {
			return err
		}
		if !allowed {
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
