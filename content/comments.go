package content

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
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

// uuidRe validates a comment id before it reaches a uuid column, so a
// malformed id is a clean 404/400 instead of a cast error.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// createInput is the POST body for a new comment.
type createInput struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id,omitempty"`
	AnonName  string `json:"anon_name,omitempty"`
}

// editInput is the PATCH body for an edit.
type editInput struct {
	Body string `json:"body"`
}

// Comment is the API view of a social_comments row.
type Comment struct {
	ID         string      `json:"id"`
	ReplyToID  string      `json:"reply_to_id,omitempty"`
	UserID     string      `json:"user_id,omitempty"`
	AnonName   string      `json:"anon_name,omitempty"`
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

// create gates on accessibility, sanitizes and screens the body and inserts.
// A reply must target a published top-level comment on the same reference;
// replies are one level deep. A held comment is stored author-only and
// counted only once a reviewer approves it.
func (c *comments) create(ctx context.Context, actor Actor, kind, id string, in createInput) (Comment, error) {
	ref, err := c.rt.gate(ctx, kind, id, actor, true)
	if err != nil {
		return Comment{}, err
	}
	key := ref.Key()

	loggedIn := actor.ID != "" && !actor.Anonymous
	var userID, anonName any
	if loggedIn {
		userID = actor.ID
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

	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(ctx)
	if err := c.rt.guardPrivateSubject(ctx, tx, viewerID(actor)); err != nil {
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
		var targetState string
		row := tx.QueryRow(ctx, `SELECT `+keyCols+`, reply_to_id::text, deleted_at, moderation FROM `+c.s.t.comments+` WHERE id = $1`, in.ReplyToID)
		if err := row.Scan(&target.TenantID, &target.ContentKind, &target.ContentID, &target.ContentVersionID, &targetReply, &targetDeleted, &targetState); err != nil {
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
		replyTo = in.ReplyToID
	}

	out := Comment{ReplyToID: in.ReplyToID, Body: clean}
	row := tx.QueryRow(ctx, `INSERT INTO `+c.s.t.comments+` (`+keyCols+`, reply_to_id, user_id, anon_name, body, moderation, moderation_reason, moderation_verdict)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id::text, created_at, updated_at`,
		append(keyArgs(key), replyTo, userID, anonName, clean, sc.state, sc.reason, sc.meta)...)
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
	} else {
		out.Moderation, out.ModerationReason = sc.state, deref(sc.reason)
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	if loggedIn {
		out.UserID = actor.ID
	} else {
		out.AnonName, _ = anonName.(string)
	}
	return out, nil
}

// cleanBody trims and sanitizes a comment body; empty after either is a 400.
func (c *comments) cleanBody(ctx context.Context, raw string) (string, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", badRequest("body is required")
	}
	clean, err := c.rt.processor.Sanitize(ctx, body)
	if err != nil {
		return "", err
	}
	if clean = strings.TrimSpace(clean); clean == "" {
		return "", badRequest("body is required")
	}
	return clean, nil
}

const commentCols = `id::text, reply_to_id::text, user_id, anon_name, body, likes, dislikes, reply_count, deleted_at, created_at, updated_at, moderation, coalesce(moderation_reason, '')`

// scanComment reads one commentCols row; state/reason are surfaced only when
// the comment is not approved (a reader only ever receives their own).
func scanComment(row pgx.Row, cm *Comment) (replyTo, userID, anonName *string, deletedAt *time.Time, err error) {
	var state, reason string
	if err = row.Scan(&cm.ID, &replyTo, &userID, &anonName, &cm.Body, &cm.Likes, &cm.Dislikes, &cm.ReplyCount, &deletedAt, &cm.CreatedAt, &cm.UpdatedAt, &state, &reason); err != nil {
		return
	}
	if state != ModerationApproved {
		cm.Moderation, cm.ModerationReason = state, reason
	}
	return
}

// list returns the reference's published top-level comments (plus the
// caller's own held/rejected ones), newest-first and paginated, each with
// reply_count. Requires the content be visible (not accessible: reading is
// allowed on premium-locked targets).
func (c *comments) list(ctx context.Context, actor Actor, kind, id, sort string, limit, offset int) ([]Comment, error) {
	ref, err := c.rt.gate(ctx, kind, id, actor, false)
	if err != nil {
		return nil, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+` FROM `+c.s.t.comments+`
		WHERE `+keyPred(1)+` AND reply_to_id IS NULL AND `+visiblePred(5)+`
		`+orderBy(sort, "likes", "dislikes", "created_at")+`
		LIMIT $6 OFFSET $7`, append(keyArgs(ref.Key()), viewerID(actor), limit, offset)...)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// replies returns a comment's direct published replies (plus the caller's
// own), oldest-first + paginated. Gated on the target content's visibility.
func (c *comments) replies(ctx context.Context, actor Actor, replyToID string, limit, offset int) ([]Comment, error) {
	if !uuidRe.MatchString(replyToID) {
		return nil, ErrNotFound
	}
	ref, err := c.refOf(ctx, c.s.pool, replyToID)
	if err != nil {
		return nil, err
	}
	if _, err := c.rt.gate(ctx, ref.ContentKind, ref.ContentID, actor, false); err != nil {
		return nil, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+` FROM `+c.s.t.comments+`
		WHERE reply_to_id = $1 AND `+visiblePred(2)+` ORDER BY created_at ASC LIMIT $3 OFFSET $4`, replyToID, viewerID(actor), limit, offset)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// refOf returns the reference a comment of this tenant belongs to.
func (c *comments) refOf(ctx context.Context, q querier, cid string) (contentref.ContentRef, error) {
	var k contentref.ContentKey
	err := q.QueryRow(ctx, `SELECT `+keyCols+` FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2`, cid, c.s.tenant).
		Scan(&k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID)
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
// longer shows to this actor, so a page may under-fill. Each distinct
// reference costs one resolver call per page.
func (c *comments) latest(ctx context.Context, actor Actor, limit, offset int) ([]FeedItem, error) {
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+`, content_kind, content_id, content_version_id
		FROM `+c.s.t.comments+` WHERE tenant_id = $1 AND deleted_at IS NULL AND moderation = 'approved'
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`, c.s.tenant, limit, offset)
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
		if err := rows.Scan(&it.ID, &replyTo, &userID, &anonName, &it.Body, &it.Likes, &it.Dislikes, &it.ReplyCount, &deletedAt, &it.CreatedAt, &it.UpdatedAt, &state, &reason, &kind, &id, &version); err != nil {
			return nil, err
		}
		it.ContentRef = contentref.NewVersion(c.s.tenant, kind, id, version)
		it.ReplyToID, it.UserID, it.AnonName = deref(replyTo), deref(userID), deref(anonName)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	visible := map[contentref.ContentKey]bool{}
	for _, it := range items {
		k := it.ContentRef.Key()
		if _, done := visible[k]; done {
			continue
		}
		_, err := c.rt.gate(ctx, it.ContentKind, it.ContentID, actor, false)
		visible[k] = err == nil
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
	if err := c.enrichAuthors(ctx, flat, authorIDs); err != nil {
		return nil, err
	}
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
// and rejected ones included with their real bodies. Optional content kind
// filter.
func (c *comments) adminList(ctx context.Context, kind string, limit, offset int) ([]AdminComment, error) {
	where, args := "", []any{c.s.tenant, limit, offset}
	if kind != "" {
		where = ` AND content_kind = $4`
		args = append(args, kind)
	}
	rows, err := c.s.pool.Query(ctx, `SELECT `+commentCols+`, content_kind, content_id, content_version_id
		FROM `+c.s.t.comments+` WHERE tenant_id = $1`+where+`
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminComment
	var flat []Comment
	var authorIDs []string
	for rows.Next() {
		var it AdminComment
		var replyTo, userID, anonName *string
		var kindCol, id, version, state, reason string
		if err := rows.Scan(&it.ID, &replyTo, &userID, &anonName, &it.Body, &it.Likes, &it.Dislikes, &it.ReplyCount, &it.DeletedAt, &it.CreatedAt, &it.UpdatedAt, &state, &reason, &kindCol, &id, &version); err != nil {
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
		flat = append(flat, it.Comment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []AdminComment{}, nil
	}
	if err := c.enrichAuthors(ctx, flat, authorIDs); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Author = flat[i].Author
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
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var replyTo *string
	var k contentref.ContentKey
	var state string
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET deleted_at = NULL, updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NOT NULL
		RETURNING reply_to_id::text, moderation, `+keyCols, cid, c.s.tenant).Scan(&replyTo, &state, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID)
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
	}
	return tx.Commit(ctx)
}

// hydrate scans comment rows (tombstoning soft-deleted ones), then batch-attaches
// authors + the caller's own reaction.
func (c *comments) hydrate(ctx context.Context, actor Actor, rows pgx.Rows) ([]Comment, error) {
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
	if err := c.enrichAuthors(ctx, out, authorIDs); err != nil {
		return nil, err
	}
	if err := c.attachMine(ctx, actor, out); err != nil {
		return nil, err
	}
	return out, nil
}

// enrichAuthors batch-loads display data for the non-tombstoned authors.
func (c *comments) enrichAuthors(ctx context.Context, list []Comment, authorIDs []string) error {
	if len(authorIDs) == 0 {
		return nil
	}
	users, err := c.rt.users.UsersByIDs(ctx, dedup(authorIDs))
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].Deleted || list[i].UserID == "" {
			continue
		}
		if u, ok := users[list[i].UserID]; ok {
			pu := u
			list[i].Author = &pu
		}
	}
	return nil
}

// attachMine sets each comment's Mine via one query over the comment kind.
func (c *comments) attachMine(ctx context.Context, actor Actor, list []Comment) error {
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
		WHERE tenant_id = $1 AND content_kind = $2 AND content_version_id = '' AND content_id = ANY($3) AND `+actorPred(userID, 4),
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
// the new text sets its state, so a held comment publishes on approval and a
// published one is withdrawn on review. Allowed for the owner or a
// moderator. 404 if missing or soft-deleted.
func (c *comments) edit(ctx context.Context, actor Actor, cid, rawBody string) (Comment, error) {
	target, err := c.loadForWrite(ctx, actor, cid)
	if err != nil {
		return Comment{}, err
	}
	clean, err := c.cleanBody(ctx, rawBody)
	if err != nil {
		return Comment{}, err
	}
	sc, err := c.rt.screen(ctx, ModerationInput{SubjectID: deref(target.ownerID), Actor: actor, Ref: target.ref, Kind: KindComment, ItemID: cid, Text: clean})
	if err != nil {
		return Comment{}, err
	}
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(ctx)
	if err := c.rt.guardPrivateSubject(ctx, tx, deref(target.ownerID)); err != nil {
		return Comment{}, err
	}
	var before string
	var revision int64
	var k contentref.ContentKey
	err = tx.QueryRow(ctx, `SELECT moderation, moderation_revision, `+keyCols+` FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR UPDATE`, cid, c.s.tenant).
		Scan(&before, &revision, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID)
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
		WHERE id = $1 RETURNING `+commentCols, cid, clean, sc.state, sc.reason, sc.meta), &cm)
	if err != nil {
		return Comment{}, err
	}
	if delta := b2i(sc.state == ModerationApproved) - b2i(before == ModerationApproved); delta != 0 {
		if err := c.count(ctx, tx, replyTo, k, delta); err != nil {
			return Comment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	cm.ReplyToID, cm.UserID, cm.AnonName = deref(replyTo), deref(userID), deref(anonName)
	return cm, nil
}

// softDelete tombstones a comment (keeps the row for thread integrity). Allowed
// for the owner or a moderator. Decrements the replied-to comment's
// reply_count / the reference's comment_count when a published comment is
// deleted.
func (c *comments) softDelete(ctx context.Context, actor Actor, cid string) error {
	if _, err := c.loadForWrite(ctx, actor, cid); err != nil {
		return err
	}
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var replyTo *string
	var k contentref.ContentKey
	var state string
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		RETURNING reply_to_id::text, moderation, `+keyCols, cid, c.s.tenant).Scan(&replyTo, &state, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID)
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
	return tx.Commit(ctx)
}

// writeTarget is what loadForWrite resolves: the owner (nil for an anonymous
// comment) and the content the comment belongs to.
type writeTarget struct {
	revision int64
	ownerID  *string
	ref      contentref.ContentRef
}

// loadForWrite resolves a live comment and authorizes actor as owner-or-moderator.
func (c *comments) loadForWrite(ctx context.Context, actor Actor, cid string) (writeTarget, error) {
	if !uuidRe.MatchString(cid) {
		return writeTarget{}, ErrNotFound
	}
	var t writeTarget
	var k contentref.ContentKey
	var deletedAt *time.Time
	row := c.s.pool.QueryRow(ctx, `SELECT user_id, `+keyCols+`, deleted_at, moderation_revision FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2`, cid, c.s.tenant)
	if err := row.Scan(&t.ownerID, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID, &deletedAt, &t.revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return writeTarget{}, ErrNotFound
		}
		return writeTarget{}, err
	}
	if deletedAt != nil {
		return writeTarget{}, ErrNotFound
	}
	t.ref = k.Ref()
	if t.ownerID != nil && actor.ID != "" && !actor.Anonymous && actor.ID == *t.ownerID {
		return t, nil
	}
	if err := c.rt.requirePerm(ctx, actor, c.rt.perms.CommentModerate); err != nil {
		return writeTarget{}, err
	}
	return t, nil
}

// reactTx writes the caller's reaction to a comment and denormalizes the split
// counter on the comment row in the same tx. The comment kind is internal, so
// no gate: just a liveness check (published, not deleted).
func (c *comments) reactTx(ctx context.Context, actor Actor, cid string, value int16) (reactionCounts, error) {
	if !uuidRe.MatchString(cid) {
		return reactionCounts{}, ErrNotFound
	}
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return reactionCounts{}, err
	}
	defer tx.Rollback(ctx)
	var deletedAt *time.Time
	var state string
	if err := tx.QueryRow(ctx, `SELECT deleted_at, moderation FROM `+c.s.t.comments+` WHERE id = $1 AND tenant_id = $2`, cid, c.s.tenant).Scan(&deletedAt, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reactionCounts{}, ErrNotFound
		}
		return reactionCounts{}, err
	}
	if deletedAt != nil || state != ModerationApproved {
		return reactionCounts{}, ErrNotFound
	}
	key := c.rt.Ref(KindComment, cid).Key()
	dLikes, dDislikes, err := c.rt.reactions.applyTx(ctx, tx, actor, key, value)
	if err != nil {
		return reactionCounts{}, err
	}
	if dLikes != 0 || dDislikes != 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+` SET likes = likes + $2, dislikes = dislikes + $3, updated_at = now() WHERE id = $1`, cid, dLikes, dDislikes); err != nil {
			return reactionCounts{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return reactionCounts{}, err
	}
	return c.rt.reactions.counts(ctx, c.s.pool, actor, key)
}

// --- HTTP ---

func (c *comments) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /{kind}/{id}/comments", c.handleList)
	mux.HandleFunc("POST /{kind}/{id}/comments", c.handleCreate)
	mux.HandleFunc("GET /comments/latest", c.handleLatest) // literal beats {cid}
	mux.HandleFunc("GET /comments/admin", c.handleAdminList)
	mux.HandleFunc("POST /comments/{cid}/restore", c.handleRestore)
	mux.HandleFunc("GET /comments/{cid}/replies", c.handleReplies)
	mux.HandleFunc("PATCH /comments/{cid}", c.handleEdit)
	mux.HandleFunc("DELETE /comments/{cid}", c.handleDelete)
	mux.HandleFunc("POST /comments/{cid}/like", c.handleReact(1))
	mux.HandleFunc("POST /comments/{cid}/dislike", c.handleReact(-1))
	mux.HandleFunc("POST /comments/{cid}/neutral", c.handleReact(0))
}

func (c *comments) handleList(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	list, err := c.list(req.Context(), actor, req.PathValue("kind"), req.PathValue("id"), req.URL.Query().Get("sort"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(list))
}

// handleAdminList serves the moderation queue (CommentModerate-gated).
func (c *comments) handleAdminList(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.rt.requirePerm(req.Context(), actor, c.rt.perms.CommentModerate); err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := parsePage(req)
	items, err := c.adminList(req.Context(), req.URL.Query().Get("content_kind"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(items))
}

func (c *comments) handleRestore(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.rt.requirePerm(req.Context(), actor, c.rt.perms.CommentModerate); err != nil {
		writeErr(w, err)
		return
	}
	if err := c.restore(req.Context(), req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restored": true})
}

func (c *comments) handleLatest(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	items, err := c.latest(req.Context(), actor, limit, offset)
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

func (c *comments) handleCreate(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	var in createInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	cm, err := c.create(req.Context(), actor, req.PathValue("kind"), req.PathValue("id"), in)
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

func (c *comments) handleEdit(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	var in editInput
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
