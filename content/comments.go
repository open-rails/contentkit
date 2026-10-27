package content

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/versionid"
	"github.com/open-rails/contentkit/kinds"
)

// comments is the threaded comment module over a content key. Threading is
// two-level: a list returns top-level comments with a reply_count, and replies
// (one level deep) are fetched lazily per comment. Split like/dislike counters
// live on the row and are bumped via reactions.applyTx. Soft-delete tombstones
// a row so a thread stays navigable.
type comments struct {
	rt *Runtime
	s  *store
}

func newComments(rt *Runtime) *comments {
	return &comments{rt: rt, s: rt.store}
}

// commentTombstone is the body shown for a soft-deleted comment.
const commentTombstone = "[deleted]"

// uuidRe validates an id before it reaches a uuid column, so a malformed id
// is a clean 404/400 instead of a cast error. Letter case does not change the
// row it names, so text keys (a comment's reactions) take the row's id::text.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CommentInput is the body of a new comment.
type CommentInput struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id,omitempty"`
	AnonName  string `json:"anon_name,omitempty"`
	// Language is the thread the comment joins: one of the site's languages,
	// or none. A reply joins its comment's.
	Language string `json:"language,omitempty"`
}

// CommentEdit is the body of a comment edit.
type CommentEdit struct {
	Body string `json:"body"`
}

// Comment is the API view of a content_comments row.
type Comment struct {
	ID         string      `json:"id"`
	ReplyToID  string      `json:"reply_to_id,omitempty"`
	UserID     string      `json:"user_id,omitempty"`
	AnonName   string      `json:"anon_name,omitempty"`
	Language   string      `json:"language,omitempty"`
	Body       string      `json:"body"`
	Deleted    bool        `json:"deleted"`
	Likes      int         `json:"likes"`
	Dislikes   int         `json:"dislikes"`
	Mine       int16       `json:"mine"` // caller's own reaction: -1/0/1
	ReplyCount int         `json:"reply_count"`
	Author     *PublicUser `json:"author,omitempty"`
	// Moderation is "held" or "rejected" on the author's own unpublished
	// comments (with the reason); absent on published ones.
	Moderation       string    `json:"moderation,omitempty"`
	ModerationReason string    `json:"moderation_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// create gates on the content opening for actor (commentsLocked) and comment
// bans, sanitizes and screens the body and inserts. A reply must target a published top-level comment on the
// same reference; replies are one level deep. A held comment is stored
// author-only and counted only once a reviewer approves it.
func (c *comments) create(ctx context.Context, actor access.Actor, target contentref.ContentRef, in CommentInput) (Comment, error) {
	if actor.Account != nil && !actor.Member() {
		return Comment{}, errForbidden
	}
	ref, res, err := c.rt.resolveTarget(ctx, target, actor, false)
	if err == nil {
		err = c.rt.commentsLocked(ref, res)
	}
	if err == nil {
		err = commentsOpen(res)
	}
	if err != nil {
		return Comment{}, err
	}
	key := ref.Key()
	if in.Language != "" && !slices.Contains(c.rt.cfg.Languages, in.Language) {
		return Comment{}, badRequest("language %q is not one of the site's", in.Language)
	}
	if err := c.rt.checkIPBan(ctx, actor); err != nil {
		return Comment{}, err
	}

	loggedIn := actor.Member()
	var userID, anonName any
	if loggedIn {
		if err := c.rt.checkCommentBan(ctx, actor.AccountID(), res); err != nil {
			return Comment{}, err
		}
		userID = actor.AccountID()
	} else {
		name := strings.TrimSpace(in.AnonName)
		if name == "" {
			return Comment{}, badRequest("anon_name is required for an anonymous comment")
		}
		anonName = name
	}

	clean, err := c.cleanBody(ctx, in.Body)
	if err != nil {
		return Comment{}, err
	}
	sc, err := c.rt.screen(ctx, ModerationInput{Actor: actor, Ref: ref, Kind: KindComment, Text: clean})
	if err != nil {
		return Comment{}, err
	}

	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(ctx)
	if err := c.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return Comment{}, err
	}

	var replyTo any
	if in.ReplyToID != "" {
		if !uuidRe.MatchString(in.ReplyToID) {
			return Comment{}, badRequest("invalid reply_to_id")
		}
		var target contentref.ContentKey
		var targetReply *string
		var targetDeleted *time.Time
		var targetState, targetLanguage string
		row := tx.QueryRow(ctx, `SELECT id::text, `+keyCols+`, reply_to_id::text, deleted_at, moderation, language FROM `+c.s.t.comments+` WHERE tenant_id = $2 AND id = $1 FOR UPDATE`, in.ReplyToID, c.s.tenant)
		if err := row.Scan(&in.ReplyToID, &target.TenantID, &target.ContentKind, &target.ContentID, versionid.Into(&target.ContentVersionID), &targetReply, &targetDeleted, &targetState, &targetLanguage); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Comment{}, badRequest("comment to reply to not found")
			}
			return Comment{}, err
		}
		if target != key || targetState != ModerationApproved { // exactly one published target
			return Comment{}, badRequest("comment to reply to not found")
		}
		if targetDeleted != nil {
			return Comment{}, badRequest("cannot reply to a deleted comment")
		}
		if targetReply != nil { // single level: replies attach to a top-level comment
			return Comment{}, badRequest("cannot reply to a reply")
		}
		if in.Language != "" && in.Language != targetLanguage {
			return Comment{}, badRequest("a reply joins its comment's language")
		}
		replyTo, in.Language = in.ReplyToID, targetLanguage
	}

	out := Comment{ReplyToID: in.ReplyToID, Body: clean, Language: in.Language}
	row := tx.QueryRow(ctx, `INSERT INTO `+c.s.t.comments+` (`+keyCols+`, reply_to_id, user_id, anon_name, body, moderation, moderation_reason, moderation_verdict, id, language)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id::text, created_at, updated_at`,
		append(keyArgs(key), replyTo, userID, anonName, clean, sc.state, sc.reason, sc.meta, contentref.NewID(), in.Language)...)
	if err := row.Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return Comment{}, err
	}
	if sc.state == ModerationApproved {
		var replyID *string
		if replyTo != nil {
			replyID = &in.ReplyToID
		}
		if err := c.count(ctx, tx, replyID, key, 1); err != nil {
			return Comment{}, err
		}
		if err := c.notifyReply(ctx, tx, out.ID); err != nil {
			return Comment{}, err
		}
	} else {
		out.Moderation, out.ModerationReason = sc.state, deref(sc.reason)
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	if loggedIn {
		out.UserID = actor.AccountID()
	} else {
		out.AnonName, _ = anonName.(string)
	}
	return c.present(ctx, actor, out), nil
}

// present attaches a written comment's author and the caller's reaction, as a
// list shows it. The write has committed, so a failed lookup leaves them out.
func (c *comments) present(ctx context.Context, actor access.Actor, cm Comment) Comment {
	list := []Comment{cm}
	var ids []string
	if cm.UserID != "" {
		ids = []string{cm.UserID}
	}
	c.enrichAuthors(ctx, list, ids)
	if err := c.attachMine(ctx, actor, list); err != nil {
		c.rt.log.WarnContext(ctx, "content: comment reaction lookup failed", "err", err.Error())
	}
	return list[0]
}

// cleanBody trims and sanitizes a comment body; empty after either is a 400,
// longer than Options.CommentMaxLength (as written) a 422 comment_too_long.
func (c *comments) cleanBody(ctx context.Context, raw string) (string, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", badRequest("body is required")
	}
	if utf8.RuneCountInString(body) > c.rt.commentMax {
		return "", &TooLongError{Max: c.rt.commentMax}
	}
	clean, err := c.rt.processor.Sanitize(ctx, body)
	if err != nil {
		return "", err
	}
	if clean = strings.TrimSpace(clean); clean == "" {
		return "", badRequest("body is required")
	}
	return c.rt.rules.apply(ctx, clean)
}

const commentCols = `id::text, reply_to_id::text, user_id, anon_name, body, likes, dislikes, reply_count, deleted_at, created_at, updated_at, moderation, coalesce(moderation_reason, ''), language`

// scanComment reads one commentCols row; state/reason are surfaced only when
// the comment is not approved (a reader only ever receives their own).
func scanComment(row pgx.Row, cm *Comment) (replyTo, userID, anonName *string, deletedAt *time.Time, err error) {
	var state, reason string
	if err = row.Scan(&cm.ID, &replyTo, &userID, &anonName, &cm.Body, &cm.Likes, &cm.Dislikes, &cm.ReplyCount, &deletedAt, &cm.CreatedAt, &cm.UpdatedAt, &state, &reason, &cm.Language); err != nil {
		return
	}
	if state != ModerationApproved {
		cm.Moderation, cm.ModerationReason = state, reason
	}
	return
}

// threadPred is shared by the top-level page and its authoritative count.
// Approved tombstones and the author's unpublished rows remain in both.
// Args are keyArgs, viewerID and language, in that order.
func threadPred() string {
	return keyPred(1) + " AND reply_to_id IS NULL AND " + visiblePred(5) + " AND ($6 = '' OR language = $6)"
}

// threadCount reads a canonical target already admitted by the caller. It
// must only be used when that target's comments are visible to the actor.
func (c *comments) threadCount(ctx context.Context, actor access.Actor, ref contentref.ContentRef, language string) (int, error) {
	var n int
	err := c.s.pool.QueryRow(ctx, `SELECT count(*) FROM `+c.s.t.comments+` WHERE `+threadPred(),
		append(keyArgs(ref.Key()), viewerID(actor), language)...).Scan(&n)
	return n, err
}

// list returns the target's published top-level comments (plus the caller's
// own held/rejected ones) in language ("" for every language), newest-first
// and paginated, each with reply_count. Requires the target be visible; on
// content the actor can't open they are withheld, like a locked body (the
// standing says why).
func (c *comments) list(ctx context.Context, actor access.Actor, target contentref.ContentRef, language, sort string, limit, offset int) ([]Comment, error) {
	ref, res, err := c.rt.resolveTarget(ctx, target, actor, false)
	if err != nil || res.Comments == CommentsHidden || !c.rt.shows(ref, res, kinds.ShowComments) {
		return []Comment{}, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+` FROM `+c.s.t.comments+`
		WHERE `+threadPred()+`
		`+orderBy(sort, "likes", "dislikes", "created_at", "id")+`
		LIMIT $7 OFFSET $8`, append(keyArgs(ref.Key()), viewerID(actor), language, limit, offset)...)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// replies returns a comment's direct published replies (plus the caller's
// own), oldest-first + paginated. Gated on the target content's visibility
// and, like list, withheld where the actor can't open it.
func (c *comments) replies(ctx context.Context, actor access.Actor, replyToID string, limit, offset int) ([]Comment, error) {
	if !uuidRe.MatchString(replyToID) {
		return nil, ErrNotFound
	}
	ref, err := c.refOf(ctx, c.s.pool, replyToID)
	if err != nil {
		return nil, err
	}
	if _, res, err := c.rt.resolveTarget(ctx, ref, actor, false); err != nil || res.Comments == CommentsHidden || !c.rt.shows(ref, res, kinds.ShowComments) {
		return []Comment{}, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+` FROM `+c.s.t.comments+`
		WHERE tenant_id = $5 AND reply_to_id = $1 AND `+visiblePred(2)+` ORDER BY created_at ASC, id ASC LIMIT $3 OFFSET $4`, replyToID, viewerID(actor), limit, offset, c.s.tenant)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// refOf returns the reference a comment of this tenant belongs to.
func (c *comments) refOf(ctx context.Context, q querier, cid string) (contentref.ContentRef, error) {
	var k contentref.ContentKey
	err := q.QueryRow(ctx, `SELECT `+keyCols+` FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2`, cid, c.s.tenant).
		Scan(&k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return contentref.ContentRef{}, ErrNotFound
	}
	return k.Ref(), err
}

// FeedItem is a Comment plus its canonical content reference, for the
// cross-content latest feed (hosts hydrate titles/covers from the reference).
type FeedItem struct {
	Comment
	contentref.ContentRef
}

// latest returns the newest published comments across all content of the
// tenant (tombstones excluded), dropping ones whose target the resolver no
// longer shows to this actor or that it can't open, so a page may under-fill. The page's distinct
// references are resolved in one resolver call.
func (c *comments) latest(ctx context.Context, actor access.Actor, language string, limit, offset int) ([]FeedItem, error) {
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+`, content_kind, content_id, content_version_id
		FROM `+c.s.t.comments+` WHERE tenant_id = $1 AND deleted_at IS NULL AND moderation = 'approved' AND ($4 = '' OR language = $4)
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, c.s.tenant, limit, offset, language)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []FeedItem
	for rows.Next() {
		var it FeedItem
		var replyTo, userID, anonName *string
		var deletedAt *time.Time
		var kind, id, version, state, reason string
		if err := rows.Scan(&it.ID, &replyTo, &userID, &anonName, &it.Body, &it.Likes, &it.Dislikes, &it.ReplyCount, &deletedAt, &it.CreatedAt, &it.UpdatedAt, &state, &reason, &it.Language, &kind, &id, versionid.Into(&version)); err != nil {
			return nil, err
		}
		it.ContentRef = contentref.NewVersion(c.s.tenant, kind, id, version)
		it.ReplyToID, it.UserID, it.AnonName = deref(replyTo), deref(userID), deref(anonName)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var refs []contentref.ContentRef
	seen := map[contentref.ContentKey]bool{}
	for _, it := range items {
		if ref := it.ContentRef; !seen[ref.Key()] && c.rt.routable(ref) {
			seen[ref.Key()] = true
			refs = append(refs, ref)
		}
	}
	visible := map[contentref.ContentKey]bool{}
	if len(refs) > 0 {
		res, err := c.rt.resolver.Resolve(ctx, refs, actor)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			_, err := c.rt.admit(ref, res, false)
			visible[ref.Key()] = err == nil && res[ref.Key()].Comments != CommentsHidden && c.rt.shows(ref, res[ref.Key()], kinds.ShowComments)
		}
	}
	kept := items[:0]
	var flat []Comment
	for _, it := range items {
		if visible[it.ContentRef.Key()] {
			kept = append(kept, it)
			flat = append(flat, it.Comment)
		}
	}
	if len(kept) == 0 {
		return []FeedItem{}, nil
	}
	var authorIDs []string
	for i := range flat {
		if flat[i].UserID != "" {
			authorIDs = append(authorIDs, flat[i].UserID)
		}
	}
	c.enrichAuthors(ctx, flat, authorIDs)
	if err := c.attachMine(ctx, actor, flat); err != nil {
		return nil, err
	}
	for i := range kept {
		kept[i].Comment = flat[i]
	}
	return kept, nil
}

// AdminComment is the moderation view: raw body (no tombstoning), deletion
// and moderation state and the content reference.
type AdminComment struct {
	Comment
	contentref.ContentRef
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// adminList returns comments newest-first for moderation, soft-deleted, held
// and rejected ones included with their real bodies, optionally only those on
// one content kind or by one author.
func (c *comments) adminList(ctx context.Context, kind, author string, limit, offset int) ([]AdminComment, error) {
	where, args := "", []any{c.s.tenant, limit, offset}
	if kind != "" {
		args = append(args, kind)
		where += fmt.Sprintf(` AND content_kind = $%d`, len(args))
	}
	if author != "" {
		args = append(args, author)
		where += fmt.Sprintf(` AND user_id = $%d`, len(args))
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+`, content_kind, content_id, content_version_id
		FROM `+c.s.t.comments+` WHERE tenant_id = $1`+where+`
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminComment
	var authorIDs []string
	for rows.Next() {
		var it AdminComment
		var replyTo, userID, anonName *string
		var kindCol, id, version, state, reason string
		if err := rows.Scan(&it.ID, &replyTo, &userID, &anonName, &it.Body, &it.Likes, &it.Dislikes, &it.ReplyCount, &it.DeletedAt, &it.CreatedAt, &it.UpdatedAt, &state, &reason, &it.Language, &kindCol, &id, versionid.Into(&version)); err != nil {
			return nil, err
		}
		it.Moderation, it.ModerationReason = state, reason
		it.ContentRef = contentref.NewVersion(c.s.tenant, kindCol, id, version)
		it.ReplyToID, it.UserID, it.AnonName = deref(replyTo), deref(userID), deref(anonName)
		if it.UserID != "" {
			authorIDs = append(authorIDs, it.UserID)
		}
		it.Deleted = it.DeletedAt != nil
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []AdminComment{}, nil
	}
	// Moderators see deleted comments' authors too.
	users := c.rt.authors(ctx, authorIDs)
	for i := range out {
		if u, ok := users[out[i].UserID]; ok && out[i].UserID != "" {
			out[i].Author = &u
		}
	}
	return out, nil
}

// restore un-deletes a tombstoned comment (moderator-only), re-incrementing the
// replied-to comment's reply_count / the reference's comment_count when it is
// a published comment.
func (c *comments) restore(ctx context.Context, cid string) error {
	if !uuidRe.MatchString(cid) {
		return ErrNotFound
	}
	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := c.lockThread(ctx, tx, cid); err != nil {
		return err
	}
	var replyTo *string
	var k contentref.ContentKey
	var state string
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET deleted_at = NULL, updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NOT NULL
		RETURNING reply_to_id::text, moderation, `+keyCols, cid, c.s.tenant).Scan(&replyTo, &state, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound // missing or not deleted
	}
	if err != nil {
		return err
	}
	if state == ModerationApproved {
		if err := c.count(ctx, tx, replyTo, k, 1); err != nil {
			return err
		}
		if err := c.restoreReplyNotices(ctx, tx, cid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// hydrate scans comment rows (tombstoning soft-deleted ones), then batch-attaches
// authors + the caller's own reaction.
func (c *comments) hydrate(ctx context.Context, actor access.Actor, rows pgx.Rows) ([]Comment, error) {
	defer rows.Close()
	var out []Comment
	var authorIDs []string
	for rows.Next() {
		var cm Comment
		replyTo, userID, anonName, deletedAt, err := scanComment(rows, &cm)
		if err != nil {
			return nil, err
		}
		cm.ReplyToID = deref(replyTo)
		if deletedAt != nil { // tombstone: keep the row, hide content + author
			cm.Deleted = true
			cm.Body = commentTombstone
		} else {
			cm.UserID, cm.AnonName = deref(userID), deref(anonName)
			if cm.UserID != "" {
				authorIDs = append(authorIDs, cm.UserID)
			}
		}
		out = append(out, cm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.enrichAuthors(ctx, out, authorIDs)
	if err := c.attachMine(ctx, actor, out); err != nil {
		return nil, err
	}
	return out, nil
}

// enrichAuthors shows the non-tombstoned authors (Runtime.authors).
func (c *comments) enrichAuthors(ctx context.Context, list []Comment, authorIDs []string) {
	users := c.rt.authors(ctx, authorIDs)
	for i := range list {
		if list[i].Deleted || list[i].UserID == "" {
			continue
		}
		if u, ok := users[list[i].UserID]; ok {
			if u.Deleted {
				// Keep ownership in storage for account recovery, but use the
				// established anonymous-author wire shape so older clients also
				// hide the account and its profile link.
				list[i].UserID, list[i].AnonName = "", "<Deleted>"
				continue
			}
			pu := u
			list[i].Author = &pu
		}
	}
}

// attachMine sets each comment's Mine via one query over the comment kind.
func (c *comments) attachMine(ctx context.Context, actor access.Actor, list []Comment) error {
	if len(list) == 0 {
		return nil
	}
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return nil
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	rows, err := c.s.pool.Query(ctx, `SELECT content_id, value FROM `+c.s.t.reactions+`
		WHERE tenant_id = $1 AND content_kind = $2 AND content_version_id IS NULL AND content_id = ANY($3) AND `+actorPred(userID, 4),
		c.s.tenant, KindComment, ids, actorArg(userID, ip))
	if err != nil {
		return err
	}
	defer rows.Close()
	mine := make(map[string]int16, len(list))
	for rows.Next() {
		var id string
		var v int16
		if err := rows.Scan(&id, &v); err != nil {
			return err
		}
		mine[id] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		list[i].Mine = mine[list[i].ID]
	}
	return nil
}

// edit re-sanitizes, re-screens and updates a comment's body: the verdict on
// the new text sets its state, so a published one is withdrawn on review and
// a held one a moderator edits publishes on approval. Allowed for the owner
// or a moderator; an owner banned in the content's scope, or who can't open
// the content, may not, nor may the owner of a held or rejected comment
// (only a reviewer's approval publishes it). 404 if missing or soft-deleted.
func (c *comments) edit(ctx context.Context, actor access.Actor, cid, rawBody string) (Comment, error) {
	target, err := c.loadForWrite(ctx, actor, cid, false)
	if err != nil {
		return Comment{}, err
	}
	author := deref(target.ownerID)
	own := author != "" && author == viewerID(actor)
	if own && target.state != ModerationApproved {
		return Comment{}, errCommentModerated
	}
	if err := c.rt.checkIPBan(ctx, actor); err != nil {
		return Comment{}, err
	}
	res, err := resolve.One(ctx, c.rt.resolver, target.ref, actor)
	if err == nil {
		err = commentsOpen(res)
	}
	if err != nil {
		return Comment{}, err
	}
	if own {
		if err := c.rt.commentsLocked(target.ref, res); err != nil {
			return Comment{}, err
		}
		if err := c.rt.checkCommentBan(ctx, author, res); err != nil {
			return Comment{}, err
		}
	}
	clean, err := c.cleanBody(ctx, rawBody)
	if err != nil {
		return Comment{}, err
	}
	sc, err := c.rt.screen(ctx, ModerationInput{SubjectID: deref(target.ownerID), Actor: actor, Ref: target.ref, Kind: KindComment, ItemID: cid, Text: clean})
	if err != nil {
		return Comment{}, err
	}
	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(ctx)
	if err := c.rt.guardErasedSubject(ctx, tx, deref(target.ownerID)); err != nil {
		return Comment{}, err
	}
	if err := c.lockThread(ctx, tx, cid); err != nil {
		return Comment{}, err
	}
	var before string
	var revision int64
	var k contentref.ContentKey
	err = tx.QueryRow(ctx, `SELECT moderation, moderation_revision, `+keyCols+` FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR UPDATE`, cid, c.s.tenant).
		Scan(&before, &revision, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, err
	}
	if revision != target.revision {
		return Comment{}, errContentChanged
	}
	var cm Comment
	replyTo, userID, anonName, _, err := scanComment(tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+`
		SET published_body = CASE WHEN $3='approved' THEN NULL WHEN moderation='approved' THEN body ELSE published_body END, body = $2, moderation_revision = moderation_revision + 1, moderated_by = NULL, moderated_at = NULL, moderation = $3, moderation_reason = $4, moderation_verdict = $5, updated_at = now()
		WHERE tenant_id = $6 AND id = $1 RETURNING `+commentCols, cid, clean, sc.state, sc.reason, sc.meta, c.s.tenant), &cm)
	if err != nil {
		return Comment{}, err
	}
	if delta := b2i(sc.state == ModerationApproved) - b2i(before == ModerationApproved); delta != 0 {
		if err := c.count(ctx, tx, replyTo, k, delta); err != nil {
			return Comment{}, err
		}
	}
	if sc.state == ModerationApproved && before != ModerationApproved {
		if err := c.restoreReplyNotices(ctx, tx, cid); err != nil {
			return Comment{}, err
		}
	} else if sc.state != ModerationApproved && before == ModerationApproved {
		if err := c.withdrawNotices(ctx, tx, cid); err != nil {
			return Comment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	cm.ReplyToID, cm.UserID, cm.AnonName = deref(replyTo), deref(userID), deref(anonName)
	return c.present(ctx, actor, cm), nil
}

// softDelete tombstones a comment (keeps the row for thread integrity). Allowed
// for the owner or a moderator. Decrements the replied-to comment's
// reply_count / the reference's comment_count when a published comment is
// deleted.
func (c *comments) softDelete(ctx context.Context, actor access.Actor, cid string) error {
	if _, err := c.loadForWrite(ctx, actor, cid, true); err != nil {
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
	var replyTo *string
	var k contentref.ContentKey
	var state string
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		RETURNING reply_to_id::text, moderation, `+keyCols, cid, c.s.tenant).Scan(&replyTo, &state, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already deleted
	}
	if err != nil {
		return err
	}
	if state == ModerationApproved {
		if err := c.count(ctx, tx, replyTo, k, -1); err != nil {
			return err
		}
	}
	if err := c.withdrawNotices(ctx, tx, cid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeTarget is what loadForWrite resolves: the owner (nil for an anonymous
// comment), its moderation state and the content the comment belongs to.
type writeTarget struct {
	revision int64
	ownerID  *string
	state    string
	ref      contentref.ContentRef
}

// loadForWrite resolves a live comment and authorizes actor as its author or
// a moderator: a root:comments:moderate holder, and with group its target's
// group comment moderators (moderates), asked only then.
func (c *comments) loadForWrite(ctx context.Context, actor access.Actor, cid string, entity bool) (writeTarget, error) {
	t, err := c.loadComment(ctx, cid)
	if err != nil {
		return t, err
	}
	if t.ownerID != nil && actor.Member() && actor.AccountID() == *t.ownerID {
		return t, nil
	}
	err = c.rt.requirePerm(ctx, actor, access.CommentsModerate)
	if err == nil || !entity || !errors.Is(err, errForbidden) {
		return t, err
	}
	return t, c.rt.moderates(ctx, actor, t.ref)
}

// loadComment reads a live comment's write target.
func (c *comments) loadComment(ctx context.Context, cid string) (writeTarget, error) {
	if !uuidRe.MatchString(cid) {
		return writeTarget{}, ErrNotFound
	}
	var t writeTarget
	var k contentref.ContentKey
	var deletedAt *time.Time
	row := c.s.pool.QueryRow(ctx, `SELECT user_id, `+keyCols+`, deleted_at, moderation_revision, moderation FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2`, cid, c.s.tenant)
	if err := row.Scan(&t.ownerID, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID), &deletedAt, &t.revision, &t.state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return writeTarget{}, ErrNotFound
		}
		return writeTarget{}, err
	}
	if deletedAt != nil {
		return writeTarget{}, ErrNotFound
	}
	t.ref = k.Ref()
	return t, nil
}

// reactTx writes the caller's reaction to a comment and denormalizes the split
// counter on the comment row in the same tx. Its owning content must be visible
// and open to the actor; the comment must still be published and not deleted
// when its row is locked.
func (c *comments) reactTx(ctx context.Context, actor access.Actor, cid string, value int16) (ReactionCounts, error) {
	if !uuidRe.MatchString(cid) {
		return ReactionCounts{}, ErrNotFound
	}
	ref, err := c.refOf(ctx, c.s.pool, cid)
	if err != nil {
		return ReactionCounts{}, err
	}
	if _, res, err := c.rt.resolveTarget(ctx, ref, actor, false); err != nil || res.Comments == CommentsHidden {
		return ReactionCounts{}, cmp.Or(err, ErrNotFound)
	} else if err := c.rt.commentsLocked(ref, res); err != nil {
		return ReactionCounts{}, err
	}
	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return ReactionCounts{}, err
	}
	defer tx.Rollback(ctx)
	if err := c.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return ReactionCounts{}, err
	}
	var deletedAt *time.Time
	var state string
	// Key by the stored id: every spelling of cid is one comment, one reaction.
	if err := tx.QueryRow(ctx, `SELECT id::text, deleted_at, moderation FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, cid, c.s.tenant).Scan(&cid, &deletedAt, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReactionCounts{}, ErrNotFound
		}
		return ReactionCounts{}, err
	}
	if deletedAt != nil || state != ModerationApproved {
		return ReactionCounts{}, ErrNotFound
	}
	key := c.rt.Ref(KindComment, cid).Key()
	dLikes, dDislikes, err := c.rt.reactions.applyTx(ctx, tx, actor, key, value)
	if err != nil {
		return ReactionCounts{}, err
	}
	if dLikes != 0 || dDislikes != 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+` SET likes = likes + $2, dislikes = dislikes + $3, updated_at = now() WHERE tenant_id = $4 AND id = $1`, cid, dLikes, dDislikes, c.s.tenant); err != nil {
			return ReactionCounts{}, err
		}
	}
	if dLikes > 0 {
		if err := c.notifyLike(ctx, tx, actor, cid, ref); err != nil {
			return ReactionCounts{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ReactionCounts{}, err
	}
	return c.rt.reactions.counts(ctx, c.s.pool, actor, key, true)
}

// --- HTTP ---

func commentRoutes() []httpapi.Route[*comments] {
	var out []httpapi.Route[*comments]
	for _, typ := range TargetTypes {
		noun := targetNoun(typ)
		withheld := ""
		if typ == TargetContent {
			withheld = " Withheld (empty) where the caller can't open the content; the standing says so."
		}
		out = append(out,
			httpapi.Route[*comments]{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/" + string(typ) + "/{id}/comments", Resource: "comments", Auth: httpapi.Public,
				Doc:       "The top-level comments on " + noun + " with reply counts; the caller also sees its own held and rejected ones." + withheld,
				Query:     targetQuery(typ, append([]httpapi.Param{languageParam, sortParam}, httpapi.Page...)...),
				Responses: []httpapi.Reply{httpapi.OK([]Comment{})}, Errors: []string{CodeNotFound}},
				Serve: func(c *comments) http.HandlerFunc { return c.rt.targetHandler(typ, c.handleList) }},
			httpapi.Route[*comments]{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/" + string(typ) + "/{id}/comments", Resource: "comments", Auth: httpapi.Public,
				Doc:     "Comments on " + noun + " in a language's thread, or replies to a top-level comment; a signed-out caller gives anon_name, where Config.anonymous.comments allows it. 202 when the moderator holds it.",
				Query:   targetQuery(typ),
				Request: CommentInput{}, Responses: []httpapi.Reply{httpapi.Created(Comment{}), httpapi.Accepted(Comment{})},
				Errors: []string{CodeCommentBanned, CodeCommentTooLong, CodeCommentsClosed, CodeCommentsLocked, CodeForbidden, CodeModerationRejected, CodeNotFound, CodeRateLimited, CodeUnauthorized}},
				Serve: func(c *comments) http.HandlerFunc { return c.handleCreate(typ) }})
	}
	return append(append(out, otherCommentRoutes...), commentModerationRoutes...)
}

var languageParam = httpapi.Text("language", "only this language's comments; every language by default")

var otherCommentRoutes = []httpapi.Route[*comments]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/comments/latest", Resource: "comments", Auth: httpapi.Public,
		Doc:   "The newest published comments across the tenant, with their targets; a page may under-fill.",
		Query: append([]httpapi.Param{languageParam}, httpapi.Page...), Responses: []httpapi.Reply{httpapi.OK([]FeedItem{})}},
		Serve: httpapi.H((*comments).handleLatest)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/comments/admin", Resource: "comments", Auth: httpapi.Staff, Perm: string(access.CommentsModerate),
		Doc: "Every comment, newest first, deleted, held and rejected ones with their real bodies.",
		Query: append([]httpapi.Param{httpapi.Text("content_kind", "only comments on this kind"),
			httpapi.Text("author", "only comments by this user id")}, httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]AdminComment{})}},
		Serve: httpapi.H((*comments).handleAdminList)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/comments/{cid}/restore", Resource: "comments", Auth: httpapi.Staff, Perm: string(access.CommentsModerate),
		Doc:       "Restores a deleted comment.",
		Responses: []httpapi.Reply{httpapi.OK(Restored{})}, Errors: []string{CodeNotFound}},
		Serve: httpapi.H((*comments).handleRestore)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/comments/{cid}/replies", Resource: "comments", Auth: httpapi.Public,
		Doc:   "A comment's replies, oldest first; withheld (empty) where the caller can't open the content.",
		Query: httpapi.Page, Responses: []httpapi.Reply{httpapi.OK([]Comment{})}, Errors: []string{CodeNotFound}},
		Serve: httpapi.H((*comments).handleReplies)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/comments/{cid}", Resource: "comments", Auth: httpapi.Public,
		Doc:       "Edits a comment: its author, or a root:comments:moderate holder. 202 when the moderator holds the new text. Its author can't edit it while it is held or rejected.",
		Request:   CommentEdit{},
		Responses: []httpapi.Reply{httpapi.OK(Comment{}), httpapi.Accepted(Comment{})},
		Errors:    []string{CodeCommentBanned, CodeCommentModerated, CodeCommentTooLong, CodeCommentsClosed, CodeCommentsLocked, CodeConflict, CodeForbidden, CodeModerationRejected, CodeNotFound}},
		Serve: httpapi.H((*comments).handleEdit)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/comments/{cid}", Resource: "comments", Auth: httpapi.Public,
		Doc:       "Deletes a comment, leaving a tombstone: its author, or a root:comments:moderate holder.",
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H((*comments).handleDelete)},
	commentReaction(httpapi.POST, "like", "Likes a comment.", 1),
	commentReaction(httpapi.POST, "dislike", "Dislikes a comment.", -1),
	commentReaction(httpapi.DELETE, "reaction", "Clears the caller's reaction to a comment.", 0),
}

func commentReaction(method, verb, doc string, value int16) httpapi.Route[*comments] {
	return httpapi.Route[*comments]{Spec: httpapi.Spec{Method: method, Path: "/comments/{cid}/" + verb, Resource: "comments", Auth: httpapi.Public,
		Doc: doc, Responses: []httpapi.Reply{httpapi.OK(ReactionCounts{})}, Errors: []string{CodeCommentsLocked, CodeForbidden, CodeInvalidRequest, CodeNotFound, CodeRateLimited, CodeUnauthorized}},
		Serve: func(c *comments) http.HandlerFunc { return c.handleReact(value) }}
}

// Restored confirms a comment's restoration.
type Restored struct {
	Restored bool `json:"restored"`
}

func (c *comments) handleList(w http.ResponseWriter, req *http.Request, target contentref.ContentRef) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	q := req.URL.Query()
	list, err := c.list(req.Context(), actor, target, q.Get("language"), q.Get("sort"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(list))
}

// handleAdminList serves the moderation queue (root:comments:moderate-gated).
func (c *comments) handleAdminList(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.rt.requirePerm(req.Context(), actor, access.CommentsModerate); err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := parsePage(req)
	q := req.URL.Query()
	items, err := c.adminList(req.Context(), q.Get("content_kind"), strings.TrimSpace(q.Get("author")), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(items))
}

func (c *comments) handleRestore(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.rt.requirePerm(req.Context(), actor, access.CommentsModerate); err != nil {
		writeErr(w, err)
		return
	}
	if err := c.restore(req.Context(), req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Restored{Restored: true})
}

func (c *comments) handleLatest(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	items, err := c.latest(req.Context(), actor, req.URL.Query().Get("language"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(items))
}

func (c *comments) handleReplies(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	list, err := c.replies(req.Context(), actor, req.PathValue("cid"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(list))
}

func (c *comments) handleCreate(typ TargetType) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := c.rt.actor(req.Context())
		err := participant(actor, c.rt.anonymous.Comments)
		if err == nil {
			err = c.rt.limit(req.Context(), ActionComment, actor)
		}
		var target contentref.ContentRef
		if err == nil {
			target, err = c.rt.target(req, typ)
		}
		var in CommentInput
		if err == nil {
			err = decodeJSON(req, &in)
		}
		var cm Comment
		if err == nil {
			cm, err = c.create(req.Context(), actor, target, in)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		status := http.StatusCreated
		if cm.Moderation == ModerationHeld { // stored, not published
			status = http.StatusAccepted
		}
		writeJSON(w, status, cm)
	}
}

func (c *comments) handleEdit(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	var in CommentEdit
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	cm, err := c.edit(req.Context(), actor, req.PathValue("cid"), in.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusOK
	if cm.Moderation == ModerationHeld {
		status = http.StatusAccepted
	}
	writeJSON(w, status, cm)
}

func (c *comments) handleDelete(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.softDelete(req.Context(), actor, req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (c *comments) handleReact(value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := c.rt.actor(req.Context())
		err := participant(actor, c.rt.anonymous.Reactions)
		if err == nil {
			err = c.rt.limit(req.Context(), ActionCommentReaction, actor)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		cnt, err := c.reactTx(req.Context(), actor, req.PathValue("cid"), value)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cnt)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// dedup returns the input with duplicate ids removed, preserving order.
func dedup(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
