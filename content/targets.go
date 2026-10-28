package content

// TargetType is what an interaction route names: a content item (optionally
// one of its versions) or an entity. Rows store the target under its kind from
// kinds.Config and its id.
type TargetType string

const (
	TargetContent TargetType = "contents"
)

const (
	TargetEntity TargetType = "entities"
)

const (
	TargetChannel TargetType = "channels"
)

const (
	CommentsOpen = "open"
)

const (
	CommentsClosed = "closed" // existing comments show; new ones and edits are refused
)

const (
	CommentsHidden = "hidden" // comments neither show nor take writes
)
