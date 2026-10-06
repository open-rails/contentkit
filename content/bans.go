package content

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
)

// Comment bans hold current state only: who is banned, in which scope, why
// and until when; no history. A ban stops every new comment by its user in
// its scope (top-level, reply and edit, on anyone's content including their
// own) and hides or deletes nothing. Reactions, favorites and votes are not
// banned; the rate limits cover them.
const (
	// ScopeGlobal is the whole tenant, managed by holders of Perms.CommentBan.
	ScopeGlobal = "global"
	ownerScope  = "owner:"
)

// OwnerScope is the scope of content whose Resolution.Owner is ownerID,
// managed by that owner.
func OwnerScope(ownerID string) string { return ownerScope + ownerID }

const (
	maxBanReason = 500 // runes
	maxBanUserID = 200 // bytes
)

// ErrCommentBanned refuses a comment write by a banned user. -> 403 comment_banned
var ErrCommentBanned = errors.New("content: banned from commenting")

// BanNotice is what a banned user is told: the scope, the reason and when the
// ban ends (nil: when lifted). It never names who banned them.
type BanNotice struct {
	Scope  string     `json:"scope"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

// BannedError is ErrCommentBanned with the ban that applies.
type BannedError struct{ BanNotice }

func (e *BannedError) Error() string {
	if e.Until != nil {
		return "you can't comment here until " + e.Until.UTC().Format(time.RFC3339)
	}
	return "you can't comment here"
}

func (e *BannedError) Is(target error) bool { return target == ErrCommentBanned }

// CommentBan is one user's ban in one scope. An expired ban stays, Expired,
// until it is lifted or replaced.
type CommentBan struct {
	UserID   string      `json:"user_id"`
	User     *PublicUser `json:"user,omitempty"`
	Scope    string      `json:"scope"`
	Reason   string      `json:"reason,omitempty"`
	Until    *time.Time  `json:"until,omitempty"`
	BannedBy string      `json:"banned_by"`
	BannedAt time.Time   `json:"banned_at"`
	Expired  bool        `json:"expired"`
}

// banInput is the PUT body; both fields are optional.
type banInput struct {
	Reason string     `json:"reason"`
	Until  *time.Time `json:"until"`
}

// checkCommentBan refuses userID's comment on content owned by ownerID ("" for
// none) when a live ban applies; of several, the one lasting longest.
func (rt *Runtime) checkCommentBan(ctx context.Context, userID, ownerID string) error {
	if userID == "" {
		return nil // anonymous commenters have no identity to ban
	}
	scopes := []string{ScopeGlobal}
	if ownerID != "" {
		scopes = append(scopes, OwnerScope(ownerID))
	}
	var n BanNotice
	err := rt.store.pool.QueryRow(ctx, `SELECT scope, reason, until FROM `+rt.store.t.commentBans+`
		WHERE tenant_id = $1 AND user_id = $2 AND scope = ANY($3) AND (until IS NULL OR until > now())
		ORDER BY until DESC NULLS FIRST, scope LIMIT 1`, rt.tenant, userID, scopes).Scan(&n.Scope, &n.Reason, &n.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &BannedError{n}
}

// banCommenter bans (or re-bans, replacing the current ban) userID in scope.
// The routes authorize who may ban which scope.
func (rt *Runtime) banCommenter(ctx context.Context, by, scope, userID string, in banInput) (CommentBan, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > maxBanUserID {
		return CommentBan{}, badRequest("invalid user id")
	}
	if userID == by {
		return CommentBan{}, badRequest("you cannot ban yourself")
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > maxBanReason {
		return CommentBan{}, badRequest("reason exceeds %d characters", maxBanReason)
	}
	if in.Until != nil && !in.Until.After(time.Now()) {
		return CommentBan{}, badRequest("until must be in the future")
	}
	tx, err := rt.store.beginMutation(ctx)
	if err != nil {
		return CommentBan{}, err
	}
	defer tx.Rollback(ctx)
	// The row names the banned user and, in an owner scope, the owner: take
	// both subject fences (sorted, as erasure does) so an erased subject's
	// data is never written again.
	subjects := []string{userID}
	if owner, ok := strings.CutPrefix(scope, ownerScope); ok {
		subjects = append(subjects, owner)
	}
	sort.Strings(subjects)
	for _, s := range subjects {
		if err := rt.guardErasedSubject(ctx, tx, s); err != nil {
			return CommentBan{}, err
		}
	}
	b := CommentBan{UserID: userID, Scope: scope, Reason: reason, BannedBy: by}
	err = tx.QueryRow(ctx, `INSERT INTO `+rt.store.t.commentBans+` (tenant_id, user_id, scope, reason, until, banned_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, user_id, scope) DO UPDATE
		SET reason = EXCLUDED.reason, until = EXCLUDED.until, banned_by = EXCLUDED.banned_by, banned_at = now()
		RETURNING until, banned_at`, rt.tenant, userID, scope, reason, in.Until, by).Scan(&b.Until, &b.BannedAt)
	if err != nil {
		return CommentBan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CommentBan{}, err
	}
	out := []CommentBan{b}
	rt.enrichBans(ctx, out)
	return out[0], nil
}

// liftCommentBan removes userID's ban in scope, expired or not; idempotent.
func (rt *Runtime) liftCommentBan(ctx context.Context, scope, userID string) error {
	_, err := rt.store.pool.Exec(ctx, `DELETE FROM `+rt.store.t.commentBans+` WHERE tenant_id = $1 AND scope = $2 AND user_id = $3`,
		rt.tenant, scope, strings.TrimSpace(userID))
	return err
}

// listCommentBans pages a scope's bans, newest first, expired ones included.
func (rt *Runtime) listCommentBans(ctx context.Context, scope string, limit, offset int) ([]CommentBan, error) {
	rows, err := rt.store.pool.Query(ctx, `SELECT user_id, scope, reason, until, banned_by, banned_at, coalesce(until <= now(), false)
		FROM `+rt.store.t.commentBans+` WHERE tenant_id = $1 AND scope = $2
		ORDER BY banned_at DESC, user_id LIMIT $3 OFFSET $4`, rt.tenant, scope, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommentBan
	for rows.Next() {
		var b CommentBan
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

// enrichBans attaches the banned users' display data; a failed lookup leaves
// bare ids.
func (rt *Runtime) enrichBans(ctx context.Context, bans []CommentBan) {
	if len(bans) == 0 {
		return
	}
	ids := make([]string, len(bans))
	for i := range bans {
		ids[i] = bans[i].UserID
	}
	users, err := rt.users.UsersByIDs(ctx, dedup(ids))
	if err != nil {
		rt.log.WarnContext(ctx, "content: comment ban users lookup failed", "err", err.Error())
		return
	}
	for i := range bans {
		if u, ok := users[bans[i].UserID]; ok {
			bans[i].User = &u
		}
	}
}

// --- HTTP ---

func (rt *Runtime) mountBans(mux *http.ServeMux) {
	for prefix, global := range map[string]bool{"/comment-bans": false, "/global-comment-bans": true} {
		mux.HandleFunc("GET "+prefix, rt.handleListBans(global))
		mux.HandleFunc("PUT "+prefix+"/{user}", rt.handleBan(global))
		mux.HandleFunc("DELETE "+prefix+"/{user}", rt.handleLiftBan(global))
	}
	mux.HandleFunc("GET /{kind}/{id}/can-comment", rt.handleCanComment)
}

// banScope authorizes the caller for one scope: the global scope needs
// Perms.CommentBan; the owner scope is the caller's own.
func (rt *Runtime) banScope(ctx context.Context, global bool) (access.Actor, string, error) {
	actor, err := rt.requireActor(ctx)
	if err != nil {
		return access.Actor{}, "", err
	}
	if !global {
		return actor, OwnerScope(actor.ID), nil
	}
	if err := rt.requirePerm(ctx, actor, rt.perms.CommentBan); err != nil {
		return access.Actor{}, "", err
	}
	return actor, ScopeGlobal, nil
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

func (rt *Runtime) handleBan(global bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor, scope, err := rt.banScope(req.Context(), global)
		if err != nil {
			writeErr(w, err)
			return
		}
		var in banInput
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

// canComment is the caller's standing on a target: may they comment, and if a
// ban stops them, which.
type canComment struct {
	CanComment bool       `json:"can_comment"`
	Ban        *BanNotice `json:"ban,omitempty"`
}

// handleCanComment answers whether the caller may comment on a visible target:
// it must be accessible to them and no ban may apply.
func (rt *Runtime) handleCanComment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	actor := rt.actor(ctx)
	_, res, err := rt.resolveTarget(ctx, req.PathValue("kind"), req.PathValue("id"), actor, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := canComment{CanComment: res.Accessible}
	var banned *BannedError
	if err := rt.checkCommentBan(ctx, viewerID(actor), res.Owner); errors.As(err, &banned) {
		out.CanComment, out.Ban = false, &banned.BanNotice
	} else if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
