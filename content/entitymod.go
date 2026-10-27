package content

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/versionid"
)

// A user-owned entity's comment moderators, its owner and the holders of
// {kind}:comments:moderate in its AuthKit scope, moderate the comments on the
// entity and on the content it owns (resolve.Resolution.Channel): they
// delete, hold and release them and ban commenters in the entity's scope.
// Each is asked only when it acts, never on reads.

// groupModerator admits actor as a comment moderator of g.
func (rt *Runtime) entityModerator(ctx context.Context, actor access.Actor, g *resolve.Channel) error {
	if g == nil {
		return errForbidden
	}
	ok, err := gate.ChannelEditor(ctx, rt.guard.Auth, actor, g.Scope, access.ChannelCommentsModerate)
	switch {
	case err != nil:
		return &AuthError{Err: err}
	case !ok:
		return errForbidden
	}
	return nil
}

// moderates admits actor as a comment moderator of ref's owner entity.
func (rt *Runtime) moderates(ctx context.Context, actor access.Actor, ref contentref.ContentRef) error {
	res, err := resolve.One(ctx, rt.resolver, ref, actor)
	if err != nil {
		return err
	}
	return rt.entityModerator(ctx, actor, res.Channel)
}

// loadForModeration resolves a live comment for a moderator: a
// root:comments:moderate holder or its target's entity comment moderators,
// not its author as such.
func (c *comments) loadForModeration(ctx context.Context, actor access.Actor, cid string) (writeTarget, error) {
	if actor.Anonymous || actor.ID == "" {
		return writeTarget{}, errUnauthorized
	}
	t, err := c.loadComment(ctx, cid)
	if err != nil {
		return t, err
	}
	err = c.rt.requirePerm(ctx, actor, access.CommentsModerate)
	if errors.Is(err, errForbidden) {
		err = c.rt.moderates(ctx, actor, t.ref)
	}
	return t, err
}

// hold takes a published comment out of its thread for review: it shows to
// its author only until released or deleted.
func (c *comments) hold(ctx context.Context, actor access.Actor, cid string) error {
	if _, err := c.loadForModeration(ctx, actor, cid); err != nil {
		return err
	}
	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := c.lockThread(ctx, tx, cid); err != nil {
		return err
	}
	var hidden bool
	var replyTo *string
	var k contentref.ContentKey
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET moderation = 'held', moderation_reason = 'held by a moderator',
		moderated_by = $3, moderated_at = now(), moderation_revision = moderation_revision + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND moderation = 'approved'
		RETURNING reply_to_id::text, author_hidden, `+keyCols, cid, c.s.tenant, actor.ID).Scan(&replyTo, &hidden, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpError{status: http.StatusConflict, msg: "only a published comment is held"}
	}
	if err != nil {
		return err
	}
	if !hidden {
		if err := c.count(ctx, tx, replyTo, k, -1); err != nil {
			return err
		}
	}
	if err := c.withdrawNotices(ctx, tx, cid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// release publishes a held comment, as a reviewer's approval does.
func (c *comments) release(ctx context.Context, actor access.Actor, cid string) error {
	t, err := c.loadForModeration(ctx, actor, cid)
	if err != nil {
		return err
	}
	return c.resolve(ctx, cid, ModerationApproved, ReviewDecision{Revision: t.revision, Decision: DecisionApprove, Reviewer: actor.ID})
}

const entityModeratorsDoc = " For root:comments:moderate holders and, on a user-owned group or its content, the group's comment moderators (its owner, holders of {kind}:comments:moderate in its AuthKit scope)."

var commentModerationRoutes = []httpapi.Route[*comments]{
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/comments/{cid}/hold", Resource: "comments", Auth: httpapi.Person,
		Doc:       "Holds a published comment for review: out of its thread, shown to its author only, until released or deleted." + entityModeratorsDoc,
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{CodeConflict, CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*comments).handleHold)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/comments/{cid}/release", Resource: "comments", Auth: httpapi.Person,
		Doc:       "Publishes a held comment, as a reviewer's approval does." + entityModeratorsDoc,
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*comments).handleRelease)},
}

func (c *comments) handleHold(w http.ResponseWriter, req *http.Request) {
	if err := c.hold(req.Context(), c.rt.actor(req.Context()), req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (c *comments) handleRelease(w http.ResponseWriter, req *http.Request) {
	if err := c.release(req.Context(), c.rt.actor(req.Context()), req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

var entityModerationRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/channels/{id}/held-comments", Resource: "moderation", Auth: httpapi.Person,
		Doc:       "Held comments on a user-owned group and the content it owns, oldest first: for root:moderation:review holders and the group's comment moderators.",
		Query:     []httpapi.Param{httpapi.Text("cursor", "the previous page's next"), httpapi.Int("limit", "page size: 20 by default, at most 100")},
		Responses: []httpapi.Reply{httpapi.OK(HeldPage{})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*Runtime).handleEntityHeld)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/channels/{id}/comment-bans", Resource: "bans", Auth: httpapi.Person,
		Doc:   "The bans on commenting on a user-owned group and the content it owns, newest first, expired ones included: for root:comments:ban holders and the group's comment moderators.",
		Query: httpapi.Page, Responses: []httpapi.Reply{httpapi.OK([]CommentBan{})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*Runtime).handleEntityBans)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/channels/{id}/comment-bans/{user}", Resource: "bans", Auth: httpapi.Person,
		Doc:     "Bans a user from commenting on a user-owned group and the content it owns, or replaces the ban: for root:comments:ban holders and the group's comment moderators.",
		Request: BanInput{}, Responses: []httpapi.Reply{httpapi.OK(CommentBan{})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*Runtime).handleEntityBan)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/channels/{id}/comment-bans/{user}", Resource: "bans", Auth: httpapi.Person,
		Doc:       "Lifts a user's ban on a user-owned group and its content.",
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*Runtime).handleEntityUnban)},
}

// groupModeration resolves the route's user-owned entity and admits the
// caller: a holder of staff, or one of the entity's comment moderators.
func (rt *Runtime) entityModeration(req *http.Request, staff access.Permission) (access.Actor, *resolve.Channel, error) {
	ctx := req.Context()
	actor, err := rt.requireActor(ctx)
	if err != nil {
		return actor, nil, err
	}
	ref, err := rt.target(req, TargetChannel)
	if err != nil {
		return actor, nil, err
	}
	res, err := resolve.One(ctx, rt.resolver, ref, actor)
	if err != nil {
		return actor, nil, err
	}
	g := res.Channel
	if !res.Visible || g == nil {
		return actor, nil, ErrNotFound
	}
	err = rt.requirePerm(ctx, actor, staff)
	if errors.Is(err, errForbidden) {
		err = rt.entityModerator(ctx, actor, g)
	}
	return actor, g, err
}

func (rt *Runtime) handleEntityHeld(w http.ResponseWriter, req *http.Request) {
	_, g, err := rt.entityModeration(req, access.ModerationReview)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, _ := parsePage(req)
	limit = min(max(limit, 1), 100)
	after, afterID, err := parseHeldCursor(req.URL.Query().Get("cursor"))
	if err != nil {
		writeErr(w, err)
		return
	}
	page := HeldPage{Items: []HeldItem{}}
	if err := rt.heldComments(req.Context(), &page, after, afterID, limit+1, g); err != nil {
		writeErr(w, err)
		return
	}
	page.trim(limit)
	rt.enrichHeld(req.Context(), page.Items)
	writeJSON(w, http.StatusOK, page)
}

func (rt *Runtime) handleEntityBans(w http.ResponseWriter, req *http.Request) {
	_, g, err := rt.entityModeration(req, access.CommentsBan)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := parsePage(req)
	bans, err := rt.listCommentBans(req.Context(), ChannelScope(g.ID), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(bans))
}

func (rt *Runtime) handleEntityBan(w http.ResponseWriter, req *http.Request) {
	actor, g, err := rt.entityModeration(req, access.CommentsBan)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in BanInput
	if req.ContentLength != 0 {
		if err := decodeJSON(req, &in); err != nil {
			writeErr(w, err)
			return
		}
	}
	b, err := rt.banCommenter(req.Context(), actor.ID, ChannelScope(g.ID), req.PathValue("user"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (rt *Runtime) handleEntityUnban(w http.ResponseWriter, req *http.Request) {
	_, g, err := rt.entityModeration(req, access.CommentsBan)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := rt.liftCommentBan(req.Context(), ChannelScope(g.ID), req.PathValue("user")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// trim cuts the page to limit, setting Next when there is more.
func (p *HeldPage) trim(limit int) {
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		last := p.Items[limit-1]
		p.Next = strconv.FormatInt(last.CreatedAt.UnixMicro(), 10) + ":" + last.ID
	}
}
