package content

// ReactionCounts is the split tally for a reference plus the caller's own
// value. The tally is absent where the kind's locked view hides it from the
// caller.
type ReactionCounts struct {
	Likes    *int  `json:"likes,omitempty"`
	Dislikes *int  `json:"dislikes,omitempty"`
	Mine     int16 `json:"mine"` // -1, 0, or 1; 0 also means "no reaction"
}
