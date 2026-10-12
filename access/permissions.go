package access

// Permission is a ContentKit staff permission: a full
// root:<resource>:<action> name, held in the root scope of the host's auth
// (auth.Root) and checked there by the Verified's Can. ContentKit names
// them; a host declares them in its auth's roles from these constants
// (AuthKit: rbac.Root.Declare) and never spells one out.
type Permission string

const (
	// ContentWrite: create, edit, publish, delete and restore any content
	// item, and see everything.
	ContentWrite Permission = "root:content:write"
	// ContentViewRestricted: see every content item, whatever its state,
	// listing, site scope or access.
	ContentViewRestricted Permission = "root:content:view-restricted"
	// PollsWrite: create, edit and delete polls, their options and images,
	// and see scheduled and inactive ones.
	PollsWrite Permission = "root:polls:write"
	// CommentsModerate: edit, delete and restore anyone's comments, and list
	// every comment.
	CommentsModerate Permission = "root:comments:moderate"
	// CommentsBan: ban users from commenting anywhere in the tenant.
	CommentsBan Permission = "root:comments:ban"
	// ModerationReview: list and resolve held comments and content.
	ModerationReview Permission = "root:moderation:review"
	// GroupsWrite: create, edit, merge and delete groups, and set content's
	// memberships.
	GroupsWrite Permission = "root:groups:write"
	// OffersWrite: put and withdraw offers on any content, version or group;
	// creators and group owners need it for nobody else's.
	OffersWrite Permission = "root:offers:write"
	// NotificationsPublish: publish notices to users' inboxes (a host service).
	NotificationsPublish Permission = "root:notifications:publish"
	// ImportWrite: bulk-import content, groups, interactions and signals (a
	// legacy importer).
	ImportWrite Permission = "root:import:write"
)

// Permissions is every staff permission.
func Permissions() []Permission {
	return []Permission{ContentWrite, ContentViewRestricted, PollsWrite, CommentsModerate, CommentsBan, ModerationReview, GroupsWrite, OffersWrite, NotificationsPublish, ImportWrite}
}

func (p Permission) String() string { return string(p) }

// Names is ps as strings, as an auth's role catalog declares them.
func Names(ps ...Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}
