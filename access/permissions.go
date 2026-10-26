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
	// ContentEditOwn: edit and publish content created by this account outside
	// a channel. Channel posts need the channel edit-own or broad write grant.
	ContentEditOwn Permission = "root:content:edit-own"
	// ContentWrite: create, edit, publish, delete and restore any content
	// item, and see everything.
	ContentWrite Permission = "root:content:write"
	// ContentViewRestricted: see every content item, whatever its state,
	// listing, site scope or access, and the site's content statistics.
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
	// EntitiesWrite: create, edit, merge and delete entities, and set content's
	// memberships.
	EntitiesWrite Permission = "root:entities:write"
	// ChannelsWrite: manage channel identity, lifecycle and configuration.
	ChannelsWrite Permission = "root:channels:write"
	// NotificationsPublish: publish notices to users' inboxes (a host service).
	NotificationsPublish Permission = "root:notifications:publish"
	// ImportWrite: bulk-import content, entities, interactions and signals (a
	// legacy importer).
	ImportWrite Permission = "root:import:write"
	// AvatarsWrite: upload, edit and remove anyone's avatar on the avatar
	// primary, exempt from limits: staff, and another site forwarding its
	// people's (AvatarsConfig.Key).
	AvatarsWrite Permission = "root:avatars:write"
)

// Permissions is every application permission; hosts choose which roles hold them.
func Permissions() []Permission {
	return []Permission{ContentCreate, ContentEditOwn, ContentWrite, ContentViewRestricted, PollsWrite, CommentsModerate, CommentsBan, ModerationReview, EntitiesWrite, ChannelsWrite, NotificationsPublish, ImportWrite, AvatarsWrite}
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

// Channel permissions are held in the primary channel's AuthKit group.
// Metadata associations never carry authority. Hosts compose roles from these
// capabilities and assign them to a channel group.
const (
	// ChannelContentCreate permits creating one's own posts in a channel.
	ChannelContentCreate = "channel:content:create"
	// ChannelContentEditOwn permits editing and publishing one's own channel posts.
	ChannelContentEditOwn = "channel:content:edit-own"
	// ChannelContentWrite permits editing every post and version in the channel.
	ChannelContentWrite = "channel:content:write"
	// ChannelSettingsManage permits changing channel settings and lifecycle.
	ChannelSettingsManage = "channel:settings:manage"
	// ChannelMediaWrite permits uploading and editing the channel's media.
	ChannelMediaWrite = "channel:media:write"
	// ChannelCommentsModerate permits moderating comments on the channel and its posts.
	ChannelCommentsModerate = "channel:comments:moderate"
)

// ChannelPermissions lists the capabilities hosts may bundle into channel roles.
func ChannelPermissions() []string {
	return []string{ChannelContentCreate, ChannelContentEditOwn, ChannelContentWrite, ChannelSettingsManage, ChannelMediaWrite, ChannelCommentsModerate}
}
