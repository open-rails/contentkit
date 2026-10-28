package entities

// MyEntity is an entity of the viewer's, with how it is theirs.
type MyEntity struct {
	EntityCard
	// Owner: the viewer is its recorded owner and retains ownership authority.
	Owner bool `json:"owner"`
	// Editor: the viewer holds its kind's access.EntitySettingsManage in its
	// AuthKit scope.
	Editor bool `json:"editor"`
	// CanPost permits new personal posts in this channel.
	CanPost bool `json:"can_post"`
	// CanEditOwn permits editing the viewer's own posts, not other authors.
	CanEditOwn bool `json:"can_edit_own"`
	// Writer: the viewer holds its kind's access.EntityContentWrite there.
	Writer bool `json:"writer"`
	// Moderator: the viewer holds its kind's access.EntityCommentsModerate
	// there, or owns it.
	Moderator bool `json:"moderator"`
	// Member: the viewer holds its key.
	Member bool `json:"member"`
}

// MyEntityPage is one page of the viewer's entities.
type MyEntityPage struct {
	Entities   []MyEntity `json:"entities"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
