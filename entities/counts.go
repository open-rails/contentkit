package entities

// Count is the number of distinct live, listed contents of one kind in a
// entity (its descendants included) or owned by it, serving one language;
// Language "" counts every language once.
type Count struct {
	ContentKind string `json:"content_kind"`
	Language    string `json:"language"`
	Count       int    `json:"count"`
}
