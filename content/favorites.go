package content

// FavoriteState says whether the caller has favorited the target, and how
// many have: absent where the target's kind's locked view hides it from the
// caller.
type FavoriteState struct {
	Favorited bool `json:"favorited"`
	Count     *int `json:"count,omitempty"`
}
