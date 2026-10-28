package entities

// Written counts the items a write named.
type Written struct {
	Count int `json:"count"`
}

// ContentMemberships is a content's memberships with the entities they name.
type ContentMemberships struct {
	ContentID   string       `json:"content_id"`
	Memberships []Membership `json:"memberships"`
	Entities    []EntityCard `json:"entities"`
}
