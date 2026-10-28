package importer

// ExternalIDsQuery asks which external ids imports under Source hold, one
// cursor-paged list per entity: what a persistent sync compares with its
// source to find the deletes to send.
type ExternalIDsQuery struct {
	Source string           `json:"source"`
	Lists  []ExternalIDList `json:"lists"`
}

// ExternalIDList asks one page of an entity's external ids.
type ExternalIDList struct {
	// Entity is content, version, entity, comment, poll or poll_option; each
	// at most once per query.
	Entity string `json:"entity"`
	// Cursor is the previous page's next_cursor.
	Cursor string `json:"cursor,omitempty"`
	// Limit defaults to 1000 and is capped at 10,000.
	Limit int `json:"limit,omitempty"`
}

// ExternalIDs answers an ExternalIDsQuery: a page per list, in order.
type ExternalIDs struct {
	Lists []ExternalIDPage `json:"lists"`
}

// ExternalIDPage is one entity's page, in external id order.
type ExternalIDPage struct {
	Entity string       `json:"entity"`
	IDs    []ExternalID `json:"ids"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ExternalID is one imported record and its id. Deleted: ContentKit holds it
// deleted or no longer holds it, so it needs no delete record.
type ExternalID struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id"`
	Deleted    bool   `json:"deleted"`
}
