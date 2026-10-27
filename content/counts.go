package content

import (
	"context"

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
