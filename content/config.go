package content

// Anonymous is what signed-out visitors may do. The zero value allows none of
// it: an anonymous attempt answers 401 unauthorized.
type Anonymous struct {
	// Comments: comment and reply under a name (anon_name).
	Comments bool `json:"comments"`
	// Reactions: like and dislike works, posts and comments, keyed by IP.
	Reactions bool `json:"reactions"`
	// Votes: signed-out votes in multiple-choice polls are keyed by IP and
	// deduplicated separately from signed-in votes. One person can vote once
	// signed out and once signed in; a changing address (e.g. IPv6) votes again.
	Votes bool `json:"votes"`
}

// Config is what the content module allows, so clients choose between a
// sign-in prompt and an anonymous form, and bound a comment, before acting.
type Config struct {
	Anonymous Anonymous `json:"anonymous"`
	// CommentMaxLength is the longest comment, in characters.
	CommentMaxLength int `json:"comment_max_length"`
}

// DefaultCommentMaxLength is Options.CommentMaxLength unset.
const DefaultCommentMaxLength = 2200
