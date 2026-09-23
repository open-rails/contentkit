package content

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// Counts is the denormalized per-reference aggregate (content_interaction_counts).
type Counts struct {
	Likes        int `json:"likes"`
	Dislikes     int `json:"dislikes"`
	Favorites    int `json:"favorites"`
	CommentCount int `json:"comment_count"`
}

// bumpCounts upserts the rollup by the given deltas inside the caller's tx.
// GREATEST clamps each count at 0 so it never goes negative.
func bumpCounts(ctx context.Context, tx pgx.Tx, s *store, key contentref.ContentKey, dLikes, dDislikes, dFav, dComments int) error {
	if dLikes == 0 && dDislikes == 0 && dFav == 0 && dComments == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.t.counts+` (`+keyCols+`, likes, dislikes, favorites, comment_count)
		VALUES ($1, $2, $3, $4, GREATEST($5,0), GREATEST($6,0), GREATEST($7,0), GREATEST($8,0))
		ON CONFLICT (`+keyCols+`) DO UPDATE SET
			likes         = GREATEST(`+s.t.counts+`.likes + $5, 0),
			dislikes      = GREATEST(`+s.t.counts+`.dislikes + $6, 0),
			favorites     = GREATEST(`+s.t.counts+`.favorites + $7, 0),
			comment_count = GREATEST(`+s.t.counts+`.comment_count + $8, 0),
			updated_at    = now()`,
		append(keyArgs(key), dLikes, dDislikes, dFav, dComments)...)
	return err
}

// orderBy builds an ORDER BY clause for a count-sortable list from a `sort`
// query value: "likes" = most likes; "best" = Wilson lower bound; else newest.
// Column names are trusted (kit-internal), never user input.
func orderBy(sort, likes, dislikes, created string) string {
	switch sort {
	case "likes":
		return "ORDER BY " + likes + " DESC, " + created + " DESC"
	case "best":
		l, d := likes, dislikes
		n := "(" + l + "+" + d + ")"
		return "ORDER BY (CASE WHEN " + n + "=0 THEN 0::float8 ELSE " +
			"((" + l + "+1.9208)/" + n + " - 1.96*sqrt((" + l + "::float8*" + d + ")/" + n + "+0.9604)/" + n + ")/(1+3.8416/" + n + ") END) DESC, " + created + " DESC"
	default:
		return "ORDER BY " + created + " DESC"
	}
}

// Counts batch-reads the aggregate counts of refs (O(1) rollup rows). A
// reference with no engagement yet is absent from the map. Preference counts
// use the canonical work; comment counts keep the original localized thread.
func (rt *Runtime) Counts(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]Counts, error) {
	out := make(map[contentref.ContentKey]Counts, len(refs))
	stored, err := rt.preferences.storedRefs(refs)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return out, nil
	}
	kinds, ids, versions := refColumns(append(append([]contentref.ContentRef(nil), refs...), stored.refs...))
	rows, err := rt.store.pool.Query(ctx, `SELECT content_kind, content_id, content_version_id, likes, dislikes, favorites, comment_count
		FROM `+rt.store.t.counts+` WHERE tenant_id = $1 AND `+refsIn(2), rt.tenant, kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		k := contentref.ContentKey{TenantID: rt.tenant}
		var c Counts
		if err := rows.Scan(&k.ContentKind, &k.ContentID, &k.ContentVersionID, &c.Likes, &c.Dislikes, &c.Favorites, &c.CommentCount); err != nil {
			return nil, err
		}
		out[k] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make(map[contentref.ContentKey]Counts, len(refs))
	for i, r := range refs {
		local, localFound := out[r.Key()]
		preference, preferenceFound := out[stored.refs[i].Key()]
		if !localFound && !preferenceFound {
			continue
		}
		local.Likes, local.Dislikes, local.Favorites = preference.Likes, preference.Dislikes, preference.Favorites
		result[r.Key()] = local
	}
	return result, nil
}

// ListFavorites returns userID's bookmarks newest-first: the host-facing Go
// API a hydrated list route reads its references from. limit <= 0 means all.
func (rt *Runtime) ListFavorites(ctx context.Context, userID string, limit, offset int) ([]FavoriteItem, error) {
	if limit <= 0 {
		limit = 1<<31 - 1
	}
	return rt.favorites.list(ctx, userID, limit, offset)
}

// LatestComments is the host-facing feed API: newest comments across all
// content the actor may see, with canonical references for host hydration.
func (rt *Runtime) LatestComments(ctx context.Context, actor Actor, limit, offset int) ([]FeedItem, error) {
	if limit <= 0 {
		limit = 20
	}
	return rt.comments.latest(ctx, actor, limit, offset)
}

// LatestCommentsTotal is the page total for LatestComments: how many live,
// approved comments the feed draws from in this tenant. Per-reference
// visibility is applied per page (a page may under-fill), so this is the
// upper bound a paged envelope reports, not a per-actor exact count —
// filtering it exactly would cost one resolver call per distinct reference
// in the whole table.
func (rt *Runtime) LatestCommentsTotal(ctx context.Context) (int, error) {
	return rt.comments.latestTotal(ctx)
}

// MyReactions batch-reads the actor's own reaction (-1/0/1) for many refs: the
// hydration read for list/detail responses, keyed by the caller's references
// and read under their canonical preference references (the identity the
// write path stores under). Only nonzero reactions appear.
func (rt *Runtime) MyReactions(ctx context.Context, actor Actor, refs []contentref.ContentRef) (map[contentref.ContentKey]int16, error) {
	out := make(map[contentref.ContentKey]int16, len(refs))
	stored, err := rt.preferences.storedRefs(refs)
	if err != nil {
		return nil, err
	}
	userID, ip, ok := reactionKey(actor)
	if !ok || len(refs) == 0 {
		return out, nil
	}
	kinds, ids, versions := refColumns(stored.refs)
	rows, err := rt.store.pool.Query(ctx, `SELECT content_kind, content_id, content_version_id, value
		FROM `+rt.store.t.reactions+`
		WHERE tenant_id = $1 AND `+actorPred(userID, 2)+` AND value <> 0 AND `+refsIn(3),
		rt.tenant, actorArg(userID, ip), kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		k := contentref.ContentKey{TenantID: rt.tenant}
		var v int16
		if err := rows.Scan(&k.ContentKind, &k.ContentID, &k.ContentVersionID, &v); err != nil {
			return nil, err
		}
		for _, caller := range stored.callers[k] {
			out[caller] = v
		}
	}
	return out, rows.Err()
}

// ActorReaction is one row of an actor's reaction history for one kind.
type ActorReaction struct {
	contentref.ContentRef
	Value     int16     `json:"value"` // -1 or 1 (neutral rows are excluded)
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReactionsByActor lists the actor's nonzero reactions of one kind,
// newest-first (e.g. a "my tag preferences" page). limit <= 0 means all.
func (rt *Runtime) ReactionsByActor(ctx context.Context, actor Actor, kind string, limit, offset int) ([]ActorReaction, error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1<<31 - 1
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT content_id, content_version_id, value, created_at, updated_at
		FROM `+rt.store.t.reactions+`
		WHERE tenant_id = $1 AND content_kind = $2 AND `+actorPred(userID, 3)+` AND value <> 0
		ORDER BY updated_at DESC LIMIT $4 OFFSET $5`,
		rt.tenant, kind, actorArg(userID, ip), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ActorReaction, 0, min(limit, 128))
	for rows.Next() {
		var r ActorReaction
		var id, version string
		if err := rows.Scan(&id, &version, &r.Value, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.ContentRef = contentref.NewVersion(rt.tenant, kind, id, version)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CommentReactionsByAuthor totals the reactions each author's published
// comments have received: the profile stat behind "likes my comments got".
// Like LatestCommentsTotal it takes no actor — held and rejected comments are
// author-only and never counted, so every reader sees the same totals, and
// filtering by per-reference visibility would cost one resolver call per
// distinct reference the author ever commented on. An author with no published
// comments is absent from the map.
func (rt *Runtime) CommentReactionsByAuthor(ctx context.Context, userIDs []string) (map[string]AuthorReactions, error) {
	return rt.comments.reactionsByAuthor(ctx, userIDs)
}

// IsFavorited batch-checks bookmarks for a user (every requested key is present
// in the map, absent bookmarks => false).
func (rt *Runtime) IsFavorited(ctx context.Context, userID string, refs []contentref.ContentRef) (map[contentref.ContentKey]bool, error) {
	return rt.favorites.IsFavorited(ctx, userID, refs)
}
