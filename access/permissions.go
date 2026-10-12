package access

// Permission is a ContentKit staff permission: a full
// root:<resource>:<action> name, held in the root scope of the host's auth
// (auth.Root) and checked there by the Verified's Can. ContentKit names
// them; a host declares them in its auth's roles from these constants
// (AuthKit: rbac.Root.Declare) and never spells one out.
type Permission string

const (
	// PostsWrite: create, edit, delete and restore posts and their images,
	// and see drafts.
	PostsWrite Permission = "root:posts:write"
	// PollsWrite: create, edit and delete polls, their options and images,
	// and see scheduled and inactive ones.
	PollsWrite Permission = "root:polls:write"
	// CommentsModerate: edit, delete and restore anyone's comments, and list
	// every comment.
	CommentsModerate Permission = "root:comments:moderate"
	// CommentsBan: ban users from commenting anywhere in the tenant.
	CommentsBan Permission = "root:comments:ban"
	// ModerationReview: list and resolve held comments and posts.
	ModerationReview Permission = "root:moderation:review"
	// GroupsWrite: create, edit, merge and delete groups, and set content's
	// memberships.
	GroupsWrite Permission = "root:groups:write"
	// OffersWrite: put and withdraw offers on any content, version or group;
	// creators and group owners need it for nobody else's.
	OffersWrite Permission = "root:offers:write"
	// NotificationsPublish: publish notices to users' inboxes (a host service).
	NotificationsPublish Permission = "root:notifications:publish"
)

// Permissions is every staff permission.
func Permissions() []Permission {
	return []Permission{PostsWrite, PollsWrite, CommentsModerate, CommentsBan, ModerationReview, GroupsWrite, OffersWrite, NotificationsPublish}
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

// GroupSettingsManage is the permission, held in a user-owned group's AuthKit
// scope (groups' auth_scope), to edit the group, delete or restore it, and
// put or withdraw its offers and its content's: "{kind}:settings:manage".
func GroupSettingsManage(kind string) string { return kind + ":settings:manage" }

// GroupMediaWrite is the permission, held in a user-owned group's AuthKit
// scope, to upload and edit the group's media: "{kind}:media:write".
func GroupMediaWrite(kind string) string { return kind + ":media:write" }

// GroupPermissions is every permission ContentKit checks in a group kind's
// AuthKit scopes, as the host's persona of that name declares them.
func GroupPermissions(kind string) []string {
	return []string{GroupSettingsManage(kind), GroupMediaWrite(kind)}
}
