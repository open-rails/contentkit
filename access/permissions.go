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
	// OffersWrite: put and withdraw offers on any content, version or entity;
	// creators and entity owners need it for nobody else's.
	OffersWrite Permission = "root:offers:write"
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

// Permissions is every staff permission.
func Permissions() []Permission {
	return []Permission{ContentWrite, ContentViewRestricted, PollsWrite, CommentsModerate, CommentsBan, ModerationReview, EntitiesWrite, OffersWrite, NotificationsPublish, ImportWrite, AvatarsWrite}
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

// Entity-scoped permissions are held in a user-owned entity's AuthKit scope
// (entities' auth_scope), whose persona is the entity kind:
// "{kind}:<resource>:<action>". A host's persona of that name declares them
// (EntityPermissions).

// EntityContentWrite: create, edit, publish and delete the content the entity
// owns (owner_entity), and see its drafts.
func EntityContentWrite(kind string) string { return kind + ":content:write" }

// EntitySettingsManage: edit the entity itself, delete or restore it, and
// put or withdraw its offers and its content's.
func EntitySettingsManage(kind string) string { return kind + ":settings:manage" }

// EntityOffersWrite: put and withdraw the entity's offers and its content's.
func EntityOffersWrite(kind string) string { return kind + ":offers:write" }

// EntityMediaWrite: upload and edit the entity's media.
func EntityMediaWrite(kind string) string { return kind + ":media:write" }

// EntityCommentsModerate: delete, hold and release comments on the entity and
// the content it owns, and ban commenters from them.
func EntityCommentsModerate(kind string) string { return kind + ":comments:moderate" }

// EntityPermissions is every permission ContentKit checks in an entity kind's
// AuthKit scopes.
func EntityPermissions(kind string) []string {
	return []string{EntityContentWrite(kind), EntitySettingsManage(kind), EntityOffersWrite(kind), EntityMediaWrite(kind), EntityCommentsModerate(kind)}
}
