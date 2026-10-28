package entities

// MembershipState: a proposed membership (a suggestion awaiting review) is
// stored but effective nowhere until it is accepted.
type MembershipState string

const (
	MembershipActive MembershipState = "active"
)

const (
	MembershipProposed MembershipState = "proposed"
)

// Membership places content, or one version of it, in an entity. Position
// orders an ordered entity (appended when omitted).
type Membership struct {
	EntityID  EntityID        `json:"group_id,omitempty"`
	ContentID string          `json:"content_id,omitempty"`
	VersionID string          `json:"version_id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Position  *int            `json:"position,omitempty"`
	State     MembershipState `json:"state,omitempty"`
}

// ContentEntities is one content's memberships in the entity kinds Kinds names.
type ContentEntities struct {
	// ContentID names the content; routes take it from the path.
	ContentID string `json:"-"`
	// VersionScope limits replacement to one ownership scope. Nil preserves
	// the legacy all-version replacement; empty means shared content links;
	// otherwise it names the one version whose links may change.
	VersionScope *string `json:"-"`
	// Kinds limits the write to these entity kinds; empty means every entity
	// kind the content's kind declares.
	Kinds       []string     `json:"kinds,omitempty"`
	Memberships []Membership `json:"memberships"`
	// Create links these names too, creating entities of creatable kinds that
	// no active entity names yet, in the same transaction.
	Create []NewEntity `json:"create,omitempty"`
}

// MemberRef names a member of an entity: content, or one version of it.
type MemberRef struct {
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

// Order sets the member order of one ordered entity.
type Order struct {
	// EntityID names the entity; routes take it from the path.
	EntityID EntityID    `json:"-"`
	Members  []MemberRef `json:"members"`
}

// MemberOptions pages Members.
type MemberOptions struct {
	// Cursor is the previous page's NextCursor.
	Cursor string
	// Limit defaults to 50 and is capped at 500.
	Limit int
}

// MemberPage is one page of an entity's memberships.
type MemberPage struct {
	Members []Membership `json:"members"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}
