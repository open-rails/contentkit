package access

// Permission is a ContentKit application permission: a full
// root:<resource>:<action> name, held in the root scope of the host's auth
// (auth.Root) and checked there by the Verified's Can. ContentKit names
// them; a host declares them in its auth's roles from these constants
// (AuthKit: rbac.Root.Declare) and never spells one out.
type Permission string

const (
	// ContentCreate: a signed-in person may create their own content outside
	// a channel. Channel posts require that channel's create or write grant.
	ContentCreate Permission = "root:content:create"
)

const (
	// ContentEditOwn: edit and publish content created by this account outside
	// a channel. Channel posts need the channel edit-own or broad write grant.
	ContentEditOwn Permission = "root:content:edit-own"
)

const (
	// ContentWrite: create, edit, publish, delete and restore any content
	// item, and see everything.
	ContentWrite Permission = "root:content:write"
)

const (
	// ContentViewRestricted: see every content item, whatever its state,
	// listing, site scope or access, and the site's content statistics.
	ContentViewRestricted Permission = "root:content:view-restricted"
)

const (
	// PollsWrite: create, edit and delete polls, their options and images,
	// and see scheduled and inactive ones.
	PollsWrite Permission = "root:polls:write"
)

const (
	// CommentsModerate: edit, delete and restore anyone's comments, and list
	// every comment.
	CommentsModerate Permission = "root:comments:moderate"
)

const (
	// CommentsBan: ban users from commenting anywhere in the tenant.
	CommentsBan Permission = "root:comments:ban"
)

const (
	// ModerationReview: list and resolve held comments and content.
	ModerationReview Permission = "root:moderation:review"
)

const (
	// EntitiesWrite: create, edit, merge and delete entities, and set content's
	// memberships.
	EntitiesWrite Permission = "root:entities:write"
)

const (
	// ChannelsWrite: manage channel identity, lifecycle and configuration.
	ChannelsWrite Permission = "root:channels:write"
)

const (
	// NotificationsPublish: publish notices to users' inboxes (a host service).
	NotificationsPublish Permission = "root:notifications:publish"
)

const (
	// ImportWrite: bulk-import content, entities, interactions and signals (a
	// legacy importer).
	ImportWrite Permission = "root:import:write"
)

const (
	// AvatarsWrite: upload, edit and remove anyone's avatar on the avatar
	// primary, exempt from limits: staff, and another site forwarding its
	// people's (AvatarsConfig.Key).
	AvatarsWrite Permission = "root:avatars:write"
)

const (
	// AccountsLifecycle: deliver AuthKit's account lifecycle (deleted,
	// restored, purged) to ContentKit over HTTP (a host service).
	AccountsLifecycle Permission = "root:accounts:lifecycle"
)

// Permissions is every application permission; hosts choose which roles hold them.
func Permissions() []Permission {
	return []Permission{ContentCreate, ContentEditOwn, ContentWrite, ContentViewRestricted, PollsWrite, CommentsModerate, CommentsBan, ModerationReview, EntitiesWrite, ChannelsWrite, NotificationsPublish, ImportWrite, AvatarsWrite, AccountsLifecycle}
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

const (
	// ChannelContentCreate permits creating one's own posts in a channel.
	ChannelContentCreate = "channel:content:create"
)

const (
	// ChannelContentEditOwn permits editing and publishing one's own channel posts.
	ChannelContentEditOwn = "channel:content:edit-own"
)

const (
	// ChannelContentWrite permits editing every post and version in the channel.
	ChannelContentWrite = "channel:content:write"
)

const (
	// ChannelSettingsManage permits changing channel settings and lifecycle.
	ChannelSettingsManage = "channel:settings:manage"
)

const (
	// ChannelMediaWrite permits uploading and editing the channel's media.
	ChannelMediaWrite = "channel:media:write"
)

const (
	// ChannelCommentsModerate permits moderating comments on the channel and its posts.
	ChannelCommentsModerate = "channel:comments:moderate"
)

// ChannelPermissions lists the capabilities hosts may bundle into channel roles.
func ChannelPermissions() []string {
	return []string{ChannelContentCreate, ChannelContentEditOwn, ChannelContentWrite, ChannelSettingsManage, ChannelMediaWrite, ChannelCommentsModerate}
}
