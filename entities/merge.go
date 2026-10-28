package entities

// Merge folds entity From into entity Into of the same kind.
type Merge struct {
	From EntityID `json:"from"`
	Into EntityID `json:"into"`
}

// MergeInput names the entity a merge folds into.
type MergeInput struct {
	Into EntityID `json:"into"`
}

// MergeReport summarizes one merge.
type MergeReport struct {
	From             EntityID `json:"from"`
	Into             EntityID `json:"into"`
	MembershipsMoved int64    `json:"memberships_moved"`
	NamesMoved       int64    `json:"names_moved"`
	EdgesMoved       int64    `json:"edges_moved"`
	EntitiesMoved    int64    `json:"groups_moved"`
}
