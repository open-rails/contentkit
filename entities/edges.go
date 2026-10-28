package entities

// Edge is a typed relation between two entities beside parent and scope:
// "from is <relation> of to", such as a character member_of a second series.
type Edge struct {
	From     EntityID `json:"from"`
	Relation string   `json:"relation"`
	To       EntityID `json:"to"`
}

// EdgeInput is an edge from the entity a route names.
type EdgeInput struct {
	Relation string   `json:"relation"`
	To       EntityID `json:"to"`
}
