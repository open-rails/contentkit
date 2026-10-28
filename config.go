package contentkit

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/userinfo"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/signal"
)

// Config is the engine's configuration: plain data.
type Config struct {
	// Scope is the business and app this engine serves. It is a trusted
	// deployment binding, not request authorization. Nil preserves legacy config.
	Scope *Scope `yaml:"scope,omitempty" json:"scope,omitempty"`
	// LegacyCatalogKey preserves an adopted app's existing row and access
	// keys, and the rollback layout's old media root. With Scope, empty uses AppID. This never renames existing data.
	LegacyCatalogKey string `yaml:"legacy_catalog_key,omitempty" json:"legacy_catalog_key,omitempty"`
	// Tenant is the legacy app/catalog key, not a business identity.
	// Deprecated: use Scope, with LegacyCatalogKey for an existing catalog.
	// It cannot be combined with Scope.
	Tenant   string         `yaml:"tenant,omitempty" json:"tenant,omitempty"`
	Database DatabaseConfig `yaml:"database" json:"database"`
	// Content is the site's content model: languages, tiers, content kinds
	// (commentable and indexed) and entity kinds. Required.
	Content kinds.Config `yaml:"content" json:"content"`
	// Interactions are what comments, reactions, favorites and polls allow.
	Interactions InteractionsConfig `yaml:"interactions" json:"interactions"`
	// Media is the media registry and how it is served; nil runs without
	// media (no upload or media route groups, no post and poll images).
	Media *MediaConfig `yaml:"media" json:"media"`
	// Avatars are people's avatars; nil: people have none. Needs Media.
	Avatars *AvatarsConfig `yaml:"avatars" json:"avatars"`
	// ClickHouse is the signal plane: signals, exposures, history,
	// preferences, popularity and recommendations from signals. Unset, or
	// while a configured one is down, they are off: the content model says
	// so (signals: false), lists sort on what Postgres holds and the
	// contentkit_clickhouse probe fails with the cause. The engine retries a
	// down one in the background and turns signals back on when it answers.
	ClickHouse ClickHouseConfig `yaml:"clickhouse" json:"clickhouse"`
	// ActivityCohorts defines application-owned daily readership buckets. First
	// match wins; nil reports totals without audience-specific categories.
	ActivityCohorts []signal.ActivityCohort `yaml:"activity_cohorts,omitempty" json:"activity_cohorts,omitempty"`
	// Accounts declares one shared account space for inboxes and avatars.
	// It does not combine this site's content catalog with any other site's.
	Accounts *AccountsConfig `json:"accounts,omitempty" yaml:"accounts,omitempty"`

	// Jobs sizes the engine's River queue.
	Jobs JobsConfig `yaml:"jobs" json:"jobs"`
	// EventRetention keeps catalog facts independently of readers; zero is ninety days.
	EventRetention time.Duration `yaml:"event_retention,omitempty" json:"event_retention,omitempty"`
}

// DatabaseConfig names the Postgres schemas New validates or migrates; the
// pool's role owns them. Shared account metadata may use its separately configured schema.
type DatabaseConfig struct {
	// SkipMigrations makes New validate storage read-only. Hosts then run
	// MigratePostgres/MigrateClickHouse from their explicit migration command.
	// False preserves the existing embedded startup migration behavior.
	SkipMigrations bool `yaml:"skip_migrations" json:"skip_migrations"`
	// Schema holds ContentKit's tables. Empty is "contentkit".
	Schema string `yaml:"schema" json:"schema"`
	// RiverSchema holds the River tables ContentKit's jobs run in; empty is
	// Schema plus "_river". Start runs ContentKit's own River client there; a
	// host fleet passed with WithRiverClient must use the same schema.
	RiverSchema string `yaml:"river_schema" json:"river_schema"`
}

// InteractionsConfig configures the content module.
type InteractionsConfig struct {
	// Anonymous is what signed-out visitors may do; zero lets them only read.
	Anonymous content.Anonymous `yaml:"anonymous" json:"anonymous"`
	// CommentMaxLength is the longest comment in characters; 0 is the default.
	CommentMaxLength int `yaml:"comment_max_length" json:"comment_max_length"`
	// DuplicateWindow refuses identical comments within this duration; zero disables it.
	DuplicateWindow time.Duration `yaml:"duplicate_window" json:"duplicate_window"`
	// Limits are the per-actor rate limits, shared through Deps.Redis.
	Limits LimitsConfig `yaml:"limits" json:"limits"`
	// ModeratorTimeout bounds one moderation call (default 5s);
	// ModeratorCooldown is how long writes are held after it failed
	// repeatedly (default 30s).
	ModeratorTimeout  time.Duration `yaml:"moderator_timeout" json:"moderator_timeout"`
	ModeratorCooldown time.Duration `yaml:"moderator_cooldown" json:"moderator_cooldown"`
}

// LimitsConfig is content.Limits as data: nil rates take the defaults.
type LimitsConfig struct {
	Disabled        bool   `yaml:"disabled" json:"disabled"`
	Comment         []Rate `yaml:"comment" json:"comment"`
	CommentReaction []Rate `yaml:"comment_reaction" json:"comment_reaction"`
	Reaction        []Rate `yaml:"reaction" json:"reaction"`
	Favorite        []Rate `yaml:"favorite" json:"favorite"`
	PollVote        []Rate `yaml:"poll_vote" json:"poll_vote"`
}

// Rate is Count actions per Per.
type Rate struct {
	Count int           `yaml:"count" json:"count"`
	Per   time.Duration `yaml:"per" json:"per"`
}

// MediaConfig is the media registry (media.Config: its Namespace is the
// tenant; content kinds' media are their versions', and each entity kind
// gains cover media, media.ForContent) and how ContentKit serves it.
type MediaConfig struct {
	media.Config `yaml:",inline"`
	// WorkerSchema holds the media worker's queue (cmd/media-worker's
	// MEDIA_WORKER_SCHEMA); empty is Database.Schema plus "_media_worker".
	WorkerSchema string         `yaml:"worker_schema" json:"worker_schema"`
	Delivery     DeliveryConfig `yaml:"delivery" json:"delivery"`
	// ProcessOnUpload has the SDK commit each upload as soon as it lands.
	ProcessOnUpload bool `yaml:"process_on_upload" json:"process_on_upload"`
	// CommitLimit limits each uploader's commits; ReadLimit each viewer's
	// reads. Zero takes media's defaults; Deps.Redis shares them.
	CommitLimit RateLimitConfig `yaml:"commit_limit" json:"commit_limit"`
	ReadLimit   RateLimitConfig `yaml:"read_limit" json:"read_limit"`
	// Issuance limits the items each viewer opens per hour.
	Issuance IssuanceConfig `yaml:"issuance" json:"issuance"`
	// Downloads are the version download quotas (anonymous, user, tiers)
	// and how long a file download's URL lasts; Deps.Redis shares them.
	Downloads versions.Downloads `yaml:"downloads" json:"downloads"`
}

// AvatarsConfig is people's avatars, keyed by the auth subject and shown
// beside comment authors and in moderation lists at URLs built from the
// person's id (the kind's Stable preset), so nothing looks one up. Of the
// sites sharing an account store, one holds them: the primary. Another
// sets Primary and keeps none: its people's avatar uploads, edits and
// editor reads go to the primary with Key, its media host serves the
// primary's files (the gateway maps the kind's namespace to the primary's
// bucket), and the primary's AnonymizeAccount erases them.
type AvatarsConfig struct {
	// Kind is the avatar kind; nil is avatar.User() (accounts/user).
	Kind *media.Kind `yaml:"kind" json:"kind"`
	// Preset is the kind's Stable public preset people are shown with;
	// empty is "avatar".
	Preset string `yaml:"preset" json:"preset"`
	// Primary is the primary's ContentKit API base, as its routes are
	// mounted ("https://doujins.dev/api/v1/contentkit"); empty: this engine
	// is the primary.
	Primary string `yaml:"primary" json:"primary"`
	// Key is an API key of the primary's auth holding root:avatars:write
	// (access.AvatarsWrite). A secret: the server reads it from the
	// environment. Required with Primary.
	Key string `yaml:"key" json:"key"`
}

// MediaKinds is the media registry Config.Media serves, in the tenant's
// namespace: content kinds' media are their versions', each entity kind
// gains cover media (media.ForContent), and the avatar primary holds the
// avatar kind (Config.Accounts.Avatars or Config.Avatars). The media worker runs it:
// cmd/media-worker reads it as JSON (`contentkit media-config` prints it),
// and a host building its own worker gives it, with its hooks, to
// media.NewRegistry.
func (c Config) MediaKinds() (media.Config, error) {
	c.Content = c.Content.StorageModel()
	var err error
	c, _, err = c.withScope()
	if err != nil {
		return media.Config{}, err
	}
	c, err = c.withAccounts()
	if err != nil {
		return media.Config{}, err
	}
	if c.Media == nil {
		return media.Config{}, errors.New("contentkit: Config.Media is not set")
	}
	mc, tenant := c.Media.Config, strings.TrimSpace(c.Tenant)
	if !mc.Scope.IsZero() {
		if mc.Namespace != "" {
			return media.Config{}, errors.New("contentkit: scoped media derives its root; remove Media.Namespace")
		}
	} else {
		switch mc.Namespace {
		case "":
			mc.Namespace = tenant
		case tenant:
		default:
			return media.Config{}, fmt.Errorf("contentkit: Config.Media.Namespace %q is not the tenant %q", mc.Namespace, tenant)
		}
	}
	if mc.Avatars != (media.Avatars{}) {
		return media.Config{}, errors.New("contentkit: people's avatars are Config.Avatars, not Config.Media.Avatars")
	}
	if a := c.Avatars; a != nil && a.Primary == "" {
		kind, preset := a.kind()
		if !mc.Scope.IsZero() {
			kind.Namespace = ""
			kind.Account = true
		}
		mc.Kinds = append(slices.Clone(mc.Kinds), kind)
		mc.Avatars = media.Avatars{Kind: kind.Name, Preset: preset}
	}
	return media.ForContent(mc, c.Content)
}

// kind is the avatar kind and preset, with their defaults.
func (a *AvatarsConfig) kind() (media.Kind, string) {
	kind, preset := avatar.User(), cmp.Or(a.Preset, "avatar")
	if a.Kind != nil {
		kind = *a.Kind
	}
	return kind, preset
}

// DeliveryConfig is how viewers with access present their item token to the
// media gateway.
type DeliveryConfig struct {
	// Mode is "url" (the default) or "cookie".
	Mode string `yaml:"mode" json:"mode"`
	// CookieDomain is the site's registrable domain; required in cookie mode.
	CookieDomain string `yaml:"cookie_domain" json:"cookie_domain"`
	// SigningKey is the media gateway's current token key, "{kid}:{base64
	// secret}"; PreviousKey the one it still verifies. A secret: the server
	// reads it from the environment. Required.
	SigningKey  string `yaml:"signing_key" json:"signing_key"`
	PreviousKey string `yaml:"previous_key" json:"previous_key"`
	// TTL is the minimum token lifetime and Window its rounding; zero takes
	// media's defaults.
	TTL    time.Duration `yaml:"ttl" json:"ttl"`
	Window time.Duration `yaml:"window" json:"window"`
}

// RateLimitConfig is media.RateLimit as data.
type RateLimitConfig struct {
	PerSecond float64 `yaml:"per_second" json:"per_second"`
	Burst     int     `yaml:"burst" json:"burst"`
	Disabled  bool    `yaml:"disabled" json:"disabled"`
}

// IssuanceConfig is media.Issuance as data.
type IssuanceConfig struct {
	PerHour          int  `yaml:"per_hour" json:"per_hour"`
	AnonymousPerHour int  `yaml:"anonymous_per_hour" json:"anonymous_per_hour"`
	Disabled         bool `yaml:"disabled" json:"disabled"`
}

// ClickHouseConfig is the signal plane's ClickHouse database.
type ClickHouseConfig struct {
	// Database is the signal plane's dedicated database; required with
	// Deps.ClickHouse or Addr.
	Database string `yaml:"database" json:"database"`
	// Addr (native protocol, "clickhouse:9000"), Username and Password let
	// New apply the signal plane's migrations, and open the connection when
	// Deps.ClickHouse is nil. Without Addr, New only checks the schema.
	Addr     string `yaml:"addr" json:"addr"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	// Cluster runs DDL and account-anonymization synchronization on the whole
	// signal replica set. It must cover one complete shard; pooled connections
	// must route only to replicas of that shard.
	Cluster string `yaml:"cluster" json:"cluster"`
}

// JobsConfig sizes the engine's River queue (QueueContentKit).
type JobsConfig struct {
	// MaxWorkers is the queue's concurrency; 0 is 2.
	MaxWorkers int `yaml:"max_workers" json:"max_workers"`
}

// Deps is everything the engine reaches outside its process: connections and
// providers. The host owns each; Close leaves them open.
type Deps struct {
	// Postgres is the pool ContentKit's schemas live in. Required.
	Postgres *pgxpool.Pool
	// ClickHouse is the signal plane's connection; nil opens one from
	// Config.ClickHouse.Addr, or runs without the signal plane.
	ClickHouse signal.Conn
	// Redis shares rate limits and issuance counts across replicas; nil
	// counts per process.
	Redis redis.UniversalClient

	// Auth says who a request is (authkit/auth; AuthKit's
	// ak.Authenticator()). It names its root scope (auth.Root), where staff
	// hold access.Permissions() in legacy mode. Required.
	Auth auth.Authenticator
	// Authorization binds verified identities and permissions to Scope.
	// Required with Config.Scope; legacy-only engines do not use it.
	Authorization Authorization
	// Users is the directory (authkit/userinfo; AuthKit's ak.UserInfo()):
	// authors' names. In scoped mode it must be restricted to Scope.TenantID
	// (AuthKit's ForTenant client); it also verifies privileged avatar targets.
	// Nil shows authors by id and refuses scoped avatar edits. The notification
	// publisher independently validates recipient ownership in its transaction.
	Users userinfo.Lookup
	// Notifications publishes native notices through the account inbox owner.
	// The publisher joins each content transaction, including remote delivery
	// obligations. Nil disables notification publication; ContentKit creates no
	// fallback inbox, receipt tables or inbox HTTP routes.
	Notifications notify.Publisher
	// Billing is the site's OpenRails merchant (adapters/openrails.Billing
	// over an embedded engine or the remote client): held entitlement keys
	// and prices, free grants. Nil holds no keys and sells nothing.
	Billing access.Billing
	// ActivitySigner optionally signs event-time audience receipts for apps
	// without media. Media apps reuse their persistent delivery signer.
	ActivitySigner discovery.ActivitySigner
	// Scopes provisions and retires the AuthKit groups of auth_scope entity
	// kinds (adapters/authkit.Scopes over an AuthKit client); nil leaves
	// auth_scope to staff.
	Scopes entities.Scopes
	// EntityReferences maintains host-owned catalog links during entity changes.
	// Nil preserves the standalone catalog behavior.
	EntityReferences entities.References
	// VersionReferences clears host-owned edition links during permanent purge.
	VersionReferences catalog.VersionReferenceOwner

	// Store is the media bucket; required with Config.Media.
	Store media.Store
	// Downloads is the host's download policy: whether a viewer may
	// download a version now, and the pages of a page download it marks.
	// Nil allows every download the access gate and quotas let through and
	// marks none.
	Downloads versions.DownloadPolicy

	// Moderator screens comments; nil publishes every comment.
	Moderator content.ContentModerator
	// ProviderDataEraser erases what a retaining Moderator
	// keeps; required with one.
	ProviderDataEraser content.ProviderDataEraser
	// Processor sanitizes comments, BodyProcessor content items' rich-text
	// bodies (keeping "contentkit:" image references); nil strips tags.
	Processor, BodyProcessor content.ContentProcessor
	// Candidates sources similar and recommended content; nil is
	// co-engagement over the signal plane.
	Candidates discovery.Candidates

	// Clock is the time; nil is time.Now.
	Clock func() time.Time
	// Logger; nil is slog.Default().
	Logger *slog.Logger
}
