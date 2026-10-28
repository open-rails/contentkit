package contentstore

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
)

const (
	ownerScope = "owner:"
)

const (
	channelScope = "group:"
)

// OwnerScope is the scope of content whose Resolution.Owner is ownerID,
// managed by that owner.
func OwnerScope(ownerID string) string { return ownerScope + ownerID }

// ChannelScope is the retained ban scope of a channel and its original posts.
// The opaque group: prefix preserves existing restrictions.
func ChannelScope(entityID string) string { return channelScope + entityID }

const (
	maxBanReason = 500 // runes
)

const (
	maxBanUserID = 200 // bytes
)

// checkCommentBan refuses userID's comment on a target resolved as res when a
// live ban applies (global, its owner's, its owner entity's); of several, the
// one lasting longest.
func (rt *Runtime) checkCommentBan(ctx context.Context, userID string, res resolve.Resolution) error {
	if userID == "" {
		return nil // anonymous commenters have no identity to ban
	}
	scopes := []string{content.ScopeGlobal}
	if res.Owner != "" {
		scopes = append(scopes, OwnerScope(res.Owner))
	}
	if g := res.Channel; g != nil {
		scopes = append(scopes, ChannelScope(g.ID))
	}
	var n content.BanNotice
	err := rt.store.pool.QueryRow(ctx, `SELECT scope, reason, until FROM `+rt.store.t.commentBans+`
		WHERE tenant_id = $1 AND user_id = $2 AND scope = ANY($3) AND (until IS NULL OR until > now())
		ORDER BY until DESC NULLS FIRST, scope LIMIT 1`, rt.tenant, userID, scopes).Scan(&n.Scope, &n.Reason, &n.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &content.BannedError{BanNotice: n}
}

// banCommenter bans (or re-bans, replacing the current ban) userID in scope.
// The routes authorize who may ban which scope.
func (rt *Runtime) banCommenter(ctx context.Context, by, scope, userID string, in content.BanInput) (content.CommentBan, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > maxBanUserID {
		return content.CommentBan{}, badRequest("invalid user id")
	}
	if userID == by {
		return content.CommentBan{}, badRequest("you cannot ban yourself")
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > maxBanReason {
		return content.CommentBan{}, badRequest("reason exceeds %d characters", maxBanReason)
	}
	if in.Until != nil && !in.Until.After(time.Now()) {
		return content.CommentBan{}, badRequest("until must be in the future")
	}
	tx, err := rt.store.beginMutation(ctx)
	if err != nil {
		return content.CommentBan{}, err
	}
	defer tx.Rollback(ctx)
	// The row names the banned user, in an owner scope the owner, and the
	// moderator: take their subject fences (sorted, as erasure does) so an
	// erased subject's id is never written again. An erased target refuses
	// the ban; an erased moderator's ban stands without their id.
	targets := []string{userID}
	if owner, ok := strings.CutPrefix(scope, ownerScope); ok {
		targets = append(targets, owner)
	}
	subjects := append([]string{by}, targets...)
	sort.Strings(subjects)
	for _, s := range subjects {
		if err := rt.lockErasureSubject(ctx, tx, s); err != nil {
			return content.CommentBan{}, err
		}
	}
	for _, s := range targets {
		if err := rt.checkSubjectNotErased(ctx, tx, s); err != nil {
			return content.CommentBan{}, err
		}
	}
	if by, err = rt.provenance(ctx, tx, by); err != nil {
		return content.CommentBan{}, err
	}
	b := content.CommentBan{UserID: userID, Scope: scope, Reason: reason, BannedBy: by}
	err = tx.QueryRow(ctx, `INSERT INTO `+rt.store.t.commentBans+` (tenant_id, user_id, scope, reason, until, banned_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, user_id, scope) DO UPDATE
		SET reason = EXCLUDED.reason, until = EXCLUDED.until, banned_by = EXCLUDED.banned_by, banned_at = now()
		RETURNING until, banned_at`, rt.tenant, userID, scope, reason, in.Until, by).Scan(&b.Until, &b.BannedAt)
	if err != nil {
		return content.CommentBan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return content.CommentBan{}, err
	}
	out := []content.CommentBan{b}
	rt.enrichBans(ctx, out)
	return out[0], nil
}

// liftCommentBan removes userID's ban in scope, expired or not; idempotent.
func (rt *Runtime) liftCommentBan(ctx context.Context, scope, userID string) error {
	_, err := rt.store.pool.Exec(ctx, `DELETE FROM `+rt.store.t.commentBans+` WHERE tenant_id = $1 AND scope = $2 AND user_id = $3`,
		rt.tenant, scope, strings.TrimSpace(userID))
	return err
}

// commentBan is userID's ban in scope, expired or not; ErrNotFound when none.
func (rt *Runtime) commentBan(ctx context.Context, scope, userID string) (content.CommentBan, error) {
	b := content.CommentBan{UserID: strings.TrimSpace(userID), Scope: scope}
	err := rt.store.pool.QueryRow(ctx, `SELECT reason, until, banned_by, banned_at, coalesce(until <= now(), false)
		FROM `+rt.store.t.commentBans+` WHERE tenant_id = $1 AND scope = $2 AND user_id = $3`, rt.tenant, scope, b.UserID).
		Scan(&b.Reason, &b.Until, &b.BannedBy, &b.BannedAt, &b.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.CommentBan{}, ErrNotFound
	}
	if err != nil {
		return content.CommentBan{}, err
	}
	out := []content.CommentBan{b}
	rt.enrichBans(ctx, out)
	return out[0], nil
}

// listCommentBans pages a scope's bans, newest first, expired ones included.
func (rt *Runtime) listCommentBans(ctx context.Context, scope string, limit, offset int) ([]content.CommentBan, error) {
	rows, err := rt.store.pool.Query(ctx, `SELECT user_id, scope, reason, until, banned_by, banned_at, coalesce(until <= now(), false)
		FROM `+rt.store.t.commentBans+` WHERE tenant_id = $1 AND scope = $2
		ORDER BY banned_at DESC, user_id LIMIT $3 OFFSET $4`, rt.tenant, scope, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []content.CommentBan
	for rows.Next() {
		var b content.CommentBan
		if err := rows.Scan(&b.UserID, &b.Scope, &b.Reason, &b.Until, &b.BannedBy, &b.BannedAt, &b.Expired); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rt.enrichBans(ctx, out)
	return out, nil
}

// enrichBans shows the banned users (Runtime.authors); a failed lookup
// leaves bare ids.
func (rt *Runtime) enrichBans(ctx context.Context, bans []content.CommentBan) {
	ids := make([]string, len(bans))
	for i := range bans {
		ids[i] = bans[i].UserID
	}
	users := rt.authors(ctx, ids)
	for i := range bans {
		if u, ok := users[bans[i].UserID]; ok {
			bans[i].User = &u
		}
	}
}

func banRoutes() []httpapi.Route[*Runtime] {
	out := append(append([]httpapi.Route[*Runtime](nil), userBanRoutes...), entityModerationRoutes...)
	for _, typ := range commentFavoriteTargets {
		out = append(out, httpapi.Route[*Runtime]{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/" + string(typ) + "/{id}/can-comment", Resource: "bans", Auth: httpapi.Public,
			Doc:       "Whether the caller may comment on " + targetNoun(typ) + ", the ban that stops it, and the authoritative visible top-level comment count. Counts include tombstones and the author's held/rejected rows; omitted when comments are hidden or locked.",
			Query:     targetQuery(typ, httpapi.Text("language", "only this language's thread; all languages by default")),
			Responses: []httpapi.Reply{httpapi.OK(content.CommentStanding{})}, Errors: []string{content.CodeNotFound}},
			Serve: func(rt *Runtime) http.HandlerFunc { return rt.targetHandler(typ, rt.handleCanComment) }})
	}
	return out
}

var userBanRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/comment-bans", Resource: "bans", Auth: httpapi.User,
		Doc:   "The bans the caller placed on commenters of its own content, newest first, expired ones included.",
		Query: httpapi.Page, Responses: []httpapi.Reply{httpapi.OK([]content.CommentBan{})}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleListBans(false) }},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/comment-bans/{user}", Resource: "bans", Auth: httpapi.User,
		Doc:       "Bans a user from commenting on the caller's content, or replaces the ban; no body bans without reason or end.",
		Request:   content.BanInput{},
		Responses: []httpapi.Reply{httpapi.OK(content.CommentBan{})}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleBan(false) }},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/comment-bans/{user}", Resource: "bans", Auth: httpapi.User,
		Doc:       "Lifts the caller's ban on a user.",
		Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleLiftBan(false) }},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/global-comment-bans", Resource: "bans", Auth: httpapi.Staff, Perm: string(access.CommentsBan),
		Doc:   "The tenant-wide comment bans, newest first, expired ones included.",
		Query: httpapi.Page, Responses: []httpapi.Reply{httpapi.OK([]content.CommentBan{})}, Errors: []string{content.CodeUnauthorized}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleListBans(true) }},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/global-comment-bans/{user}", Resource: "bans", Auth: httpapi.Staff, Perm: string(access.CommentsBan),
		Doc:       "A user's tenant-wide comment ban, expired or not; 404 when they have none.",
		Responses: []httpapi.Reply{httpapi.OK(content.CommentBan{})}, Errors: []string{content.CodeNotFound, content.CodeUnauthorized}},
		Serve: httpapi.H((*Runtime).handleGlobalBan)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/global-comment-bans/{user}", Resource: "bans", Auth: httpapi.Staff, Perm: string(access.CommentsBan),
		Doc:       "Bans a user from commenting anywhere in the tenant, or replaces the ban.",
		Request:   content.BanInput{},
		Responses: []httpapi.Reply{httpapi.OK(content.CommentBan{})}, Errors: []string{content.CodeUnauthorized}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleBan(true) }},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/global-comment-bans/{user}", Resource: "bans", Auth: httpapi.Staff, Perm: string(access.CommentsBan),
		Doc:       "Lifts a tenant-wide comment ban.",
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{content.CodeUnauthorized}},
		Serve: func(rt *Runtime) http.HandlerFunc { return rt.handleLiftBan(true) }},
}

// banScope authorizes the caller for one scope: the global scope needs
// root:comments:ban; the owner scope is the caller's own.
func (rt *Runtime) banScope(ctx context.Context, global bool) (access.Actor, string, error) {
	actor, err := rt.requireActor(ctx)
	if err != nil {
		return access.Actor{}, "", err
	}
	if !global {
		if !actor.Member() {
			return access.Actor{}, "", errForbidden
		}
		return actor, OwnerScope(actor.AccountID()), nil
	}
	if err := rt.requirePerm(ctx, actor, access.CommentsBan); err != nil {
		return access.Actor{}, "", err
	}
	return actor, content.ScopeGlobal, nil
}

func (rt *Runtime) handleListBans(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_, scope, err := rt.banScope(req.Context(), global)
		if err != nil {
			writeErr(w, err)
			return
		}
		limit, offset := parsePage(req)
		bans, err := rt.listCommentBans(req.Context(), scope, limit, offset)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, orEmpty(bans))
	}
}

func (rt *Runtime) handleGlobalBan(w http.ResponseWriter, req *http.Request) {
	_, scope, err := rt.banScope(req.Context(), true)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := rt.commentBan(req.Context(), scope, req.PathValue("user"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (rt *Runtime) handleBan(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor, scope, err := rt.banScope(req.Context(), global)
		if err != nil {
			writeErr(w, err)
			return
		}
		var in content.BanInput
		if req.ContentLength != 0 { // both fields are optional: no body is {}
			if err := decodeJSON(req, &in); err != nil {
				writeErr(w, err)
				return
			}
		}
		b, err := rt.banCommenter(req.Context(), actor.ID, scope, req.PathValue("user"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

func (rt *Runtime) handleLiftBan(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_, scope, err := rt.banScope(req.Context(), global)
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := rt.liftCommentBan(req.Context(), scope, req.PathValue("user")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusNoContent, nil)
	}
}

// handleCanComment answers whether the caller may comment on a visible target
// (it must open it, be signed in unless anonymous comments are on, and no ban
// may apply) and what it may do to the comments there.
func (rt *Runtime) handleCanComment(w http.ResponseWriter, req *http.Request, target contentref.ContentRef) {
	ctx := req.Context()
	actor := rt.actor(ctx)
	ref, res, err := rt.resolveTarget(ctx, target, actor, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := content.CommentStanding{UserID: viewerID(actor), Anonymous: rt.anonymous.Comments, MaxLength: rt.commentMax, BanScopes: []content.BanScope{},
		Comments: cmp.Or(res.Comments, content.CommentsOpen)}
	out.Locked = !rt.shows(ref, res, kinds.ShowComments)
	out.CanComment = !out.Locked && (out.UserID != "" || out.Anonymous) && out.Comments == content.CommentsOpen
	err = rt.checkCommentBan(ctx, viewerID(actor), res)
	if err == nil {
		err = rt.checkIPBan(ctx, actor)
	}
	var banned *content.BannedError
	if errors.As(err, &banned) {
		out.CanComment, out.Ban = false, &banned.BanNotice
	} else if err != nil {
		writeErr(w, err)
		return
	}
	if out.UserID != "" {
		out.Moderate = rt.requirePerm(ctx, actor, access.CommentsModerate) == nil
		if res.Owner == out.UserID {
			out.BanScopes = append(out.BanScopes, content.BanOwner)
		}
		if rt.requirePerm(ctx, actor, access.CommentsBan) == nil {
			out.BanScopes = append(out.BanScopes, content.BanGlobal)
		}
	}
	if !out.Locked && out.Comments != content.CommentsHidden {
		n, err := rt.comments.threadCount(ctx, actor, ref, req.URL.Query().Get("language"))
		if err != nil {
			writeErr(w, err)
			return
		}
		out.Count = &n
	}
	writeJSON(w, http.StatusOK, out)
}
