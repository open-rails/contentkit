package content

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
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

// RatingSQL is the Wilson lower bound (95%) of likes among likes and
// dislikes: the "best" order of comments and the rating of content. Its
// arguments are trusted column expressions, never user input.
func RatingSQL(likes, dislikes string) string {
	l, d := likes, dislikes
	n := "(" + l + "+" + d + ")"
	return "(CASE WHEN " + n + "=0 THEN 0::float8 ELSE " +
		"((" + l + "+1.9208)/" + n + " - 1.96*sqrt((" + l + "::float8*" + d + ")/" + n + "+0.9604)/" + n + ")/(1+3.8416/" + n + ") END)"
}

// orderBy builds an ORDER BY clause for a count-sortable list from a `sort`
// query value: "likes" = most likes; "best" = Wilson lower bound; else newest.
// id breaks ties so offset pages neither repeat nor skip rows. Column names
// are trusted (kit-internal), never user input.
func orderBy(sort, likes, dislikes, created, id string) string {
	switch sort {
	case "likes":
		return "ORDER BY " + likes + " DESC, " + created + " DESC, " + id + " DESC"
	case "best":
		return "ORDER BY " + RatingSQL(likes, dislikes) + " DESC, " + created + " DESC, " + id + " DESC"
	default:
		return "ORDER BY " + created + " DESC, " + id + " DESC"
	}
}

// Counts batch-reads the aggregate counts of refs (O(1) rollup rows). A
// reference with no engagement yet is absent from the map.
func (rt *Runtime) Counts(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]Counts, error) {
	out := make(map[contentref.ContentKey]Counts, len(refs))
	for _, r := range refs {
		if err := rt.checkRef(r); err != nil {
			return nil, err
		}
	}
	if len(refs) == 0 {
		return out, nil
	}
	kinds, ids, versions := refColumns(refs)
	rows, err := rt.store.pool.Query(ctx, `SELECT content_kind, content_id, content_version_id, likes, dislikes, favorites, comment_count
		FROM `+rt.store.t.counts+` WHERE tenant_id = $1 AND `+refsIn(2), rt.tenant, kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		k := contentref.ContentKey{TenantID: rt.tenant}
		var c Counts
		if err := rows.Scan(&k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID), &c.Likes, &c.Dislikes, &c.Favorites, &c.CommentCount); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// LatestComments is the feed: newest comments across all content the actor
// may see, in language ("" for every language).
func (rt *Runtime) LatestComments(ctx context.Context, actor access.Actor, language string, limit, offset int) ([]FeedItem, error) {
	if limit <= 0 {
		limit = 20
	}
	return rt.comments.latest(ctx, actor, language, limit, offset)
}

// LatestCommentsTotal is the page total for LatestComments: how many live,
// approved comments the feed draws from in this tenant. Per-reference
// visibility is applied per page (a page may under-fill), so this is the
// upper bound a paged envelope reports, not a per-actor exact count.
func (rt *Runtime) LatestCommentsTotal(ctx context.Context) (int, error) {
	return rt.comments.latestTotal(ctx)
}

// CommentReactionsByAuthor totals the reactions each author's published
// comments have received. Held and rejected comments are author-only and
// never counted, so every reader sees the same totals. An author with no
// published comments is absent from the map.
func (rt *Runtime) CommentReactionsByAuthor(ctx context.Context, userIDs []string) (map[string]AuthorReactions, error) {
	return rt.comments.reactionsByAuthor(ctx, userIDs)
}
