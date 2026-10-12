package contentkit

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/userinfo"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/signal"
)

// Config is the engine's configuration: plain data.
type Config struct {
	// Tenant is the one tenant this engine serves: the first column of every
	// key and query, and the prefix of every access key. Required.
	Tenant   string         `yaml:"tenant" json:"tenant"`
	Database DatabaseConfig `yaml:"database" json:"database"`
	// Content is the site's content model: languages, tiers, content kinds
	// (commentable and indexed) and group kinds. Required.
	Content kinds.Config `yaml:"content" json:"content"`
	// Interactions are what comments, reactions, favorites and polls allow.
	Interactions InteractionsConfig `yaml:"interactions" json:"interactions"`
	// Media is the media registry and how it is served; nil runs without
	// media (no upload or media route groups, no post and poll images).
	Media *MediaConfig `yaml:"media" json:"media"`
	// ClickHouse is the signal plane: signals, preferences, popularity and
	// discovery. Without it they are off.
	ClickHouse ClickHouseConfig `yaml:"clickhouse" json:"clickhouse"`
	// Jobs sizes the engine's River queue.
	Jobs JobsConfig `yaml:"jobs" json:"jobs"`
}

// DatabaseConfig names the Postgres schemas New creates or upgrades; the
// pool's role owns them. ContentKit never reads or writes another schema.
type DatabaseConfig struct {
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
	PostReaction    []Rate `yaml:"post_reaction" json:"post_reaction"`
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
// tenant, its hooks come from Deps) and how ContentKit serves it.
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
	// Cluster runs the migrations' DDL ON CLUSTER.
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
	// hold access.Permissions(). Required.
	Auth auth.Authenticator
	// Users is the directory (authkit/userinfo; AuthKit's ak.UserInfo()):
	// authors' names. Nil shows authors by id.
	Users userinfo.Lookup
	// Billing answers which access keys a subject holds (adapters/openrails
	// over an embedded engine or the remote client).
	Billing access.Entitlements

	// Store is the media bucket; required with Config.Media.
	Store media.Store
	// PurgePublic hears of public media URLs overwritten or deleted, for a
	// CDN purge.
	PurgePublic func(ctx context.Context, urls []string)

	// Moderator screens comment and post writes; nil publishes everything.
	Moderator content.ContentModerator
	// Classifier groups free-text poll answers; nil refuses free-text polls.
	Classifier content.AnswerClassifier
	// ProviderDataEraser erases what a retaining Moderator or Classifier
	// keeps; required with one.
	ProviderDataEraser content.ProviderDataEraser
	// Processor sanitizes comments and post excerpts, PostBodyProcessor post
	// bodies; nil strips tags.
	Processor, PostBodyProcessor content.ContentProcessor
	// Candidates sources similar and recommended content; nil is
	// co-engagement over the signal plane.
	Candidates discovery.Candidates

	// Clock is the time; nil is time.Now.
	Clock func() time.Time
	// Logger; nil is slog.Default().
	Logger *slog.Logger

	// Resolver and CanUpload answer for content the host still owns until
	// ContentKit stores it (#120 content, #121 access); each lane deletes its
	// hook. Nil Resolver resolves nothing but ContentKit's own posts and polls.
	Resolver  access.ContentResolver
	CanUpload media.UploadAuthorizer
}
