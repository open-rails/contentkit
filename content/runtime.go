package content

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/notification"
)

// KindComment is the reserved content kind of comments: reactions on comments
// are keyed by it and never pass through the resolve.Resolver.
const KindComment = "comment"

// Options configures a Runtime. Pool, Schema, Tenant, Auth and Resolver are
// mandatory; the rest fall back to documented defaults.
type Options struct {
	// Pool is the host's shared pgx pool; the runtime does not own its lifecycle.
	Pool *pgxpool.Pool
	// Schema is ContentKit's schema.
	Schema string
	// Tenant scopes every row, index, cursor and result. Required.
	Tenant string

	// Auth says who a request is (authkit/auth; AuthKit's ak.Authenticator()).
	// The routes ask it at most once per request and build the actor from
	// its Verified: anonymous without a credential, 401 for one that fails.
	// Staff hold ContentKit's permissions (access.Permissions) in its root
	// scope (auth.Root), checked by the Verified's Can: declare them in its
	// roles. Required.
	Auth auth.Authenticator
	// UserInfo is the host's directory (authkit/userinfo; AuthKit's
	// ak.UserInfo()): authors' names and usernames, read once per response
	// and never copied. Nil shows authors by id.
	UserInfo userinfo.Lookup
	// Resolver decides interaction targets: content items and their versions
	// (the catalog's) and groups. Required.
	Resolver resolve.Resolver
	// Kinds is the site's content model: interactions target its content
	// kinds and group kinds, and comments carry one of its languages.
	Kinds kinds.Config

	// PreferenceSyncOverlap bounds how long a preference write may take to
	// commit and still be picked up by SyncPreferences.
	// Default DefaultPreferenceSyncOverlap.
	PreferenceSyncOverlap time.Duration

	// Optional ports (nil -> default).
	Processor ContentProcessor // comments; default: strip tags
	// Moderator screens comment writes, and the catalog's (Screen); nil
	// publishes everything.
	// Compose a BasicModerator in front of an AI moderator with Chain.
	Moderator ContentModerator
	// ModeratorTimeout bounds one Screen call (default 5s); a slower
	// moderator counts as failed and the write is held.
	ModeratorTimeout time.Duration
	// ModeratorCooldown is how long writes are held without calling the
	// moderator after it failed repeatedly (default 30s); see CheckModerator.
	ModeratorCooldown time.Duration
	// Classifier groups free-text poll answers; nil refuses free-text polls.
	Classifier AnswerClassifier

	// Media stores poll images in ContentKit media; nil refuses image writes
	// with 501 not_configured. See Media.
	Media *Media

	// ProviderDataEraser is required when moderator/classifier ports retain
	// external personal data.
	// Nil explicitly means stateless ports; never remove it during an outage.
	ProviderDataEraser ProviderDataEraser

	// Limits are the per-actor interaction rate limits (limits.go); the zero
	// value takes the defaults, counted per process.
	Limits Limits

	// Anonymous is what signed-out visitors may do; the zero value lets them
	// only read.
	Anonymous Anonymous
	// CommentMaxLength is the longest comment in characters (runes);
	// default DefaultCommentMaxLength.
	CommentMaxLength int

	// Logger receives the access log: each request at DEBUG, a 500 at ERROR
	// with its cause. nil -> slog.Default().
	Logger *slog.Logger
}

// Runtime is one tenant's embedded content module: shared deps + the module
// services, exposing one mountable http.Handler.
type Runtime struct {
	// Notifications is this tenant's user inbox.
	Notifications  *notification.Store
	store          *store
	schema         string
	tenant         string
	guard          *gate.Gate
	userInfo       userinfo.Lookup
	resolver       resolve.Resolver
	media          *Media
	processor      ContentProcessor
	moderator      ContentModerator
	breaker        *moderatorBreaker
	classifier     AnswerClassifier
	providerEraser ProviderDataEraser
	anonymous      Anonymous
	commentMax     int
	log            *slog.Logger
	cfg            kinds.Config
	rules          *rules
	limiters       map[Action]*ratelimit.Limiter

	preferences *preferences
	reactions   *reactions
	polls       *polls
	comments    *comments
	favorites   *favorites
	held        map[string]HeldSource
}

// New constructs a Runtime over a schema the ContentKit baseline was applied to and wires the module services.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("content: Pool is required")
	}
	if strings.TrimSpace(opts.Schema) == "" {
		return nil, fmt.Errorf("content: Schema is required")
	}
	if strings.TrimSpace(opts.Tenant) == "" {
		return nil, fmt.Errorf("content: Tenant is required")
	}
	if err := gate.Check(opts.Auth, permissions...); err != nil {
		return nil, fmt.Errorf("content: Auth: %w", err)
	}
	if opts.Resolver == nil {
		return nil, fmt.Errorf("content: Resolver is required")
	}
	if err := opts.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("content: Kinds: %w", err)
	}
	_, content := opts.Kinds.Kinds[KindComment]
	if _, group := opts.Kinds.GroupKinds[KindComment]; content || group {
		return nil, fmt.Errorf("content: kind %q is reserved", KindComment)
	}
	if opts.ProviderDataEraser == nil && (!policyIsStateless(opts.Moderator) || !policyIsStateless(opts.Classifier)) {
		return nil, fmt.Errorf("content: retaining policy ports require ProviderDataEraser; stateless ports must declare StatelessPolicy")
	}
	if opts.CommentMaxLength < 0 {
		return nil, fmt.Errorf("content: CommentMaxLength must not be negative")
	}
	media, err := newMedia(opts.Media)
	if err != nil {
		return nil, err
	}
	log := orDefault[*slog.Logger](opts.Logger, slog.Default())
	limiters, err := newLimiters(opts.Limits, opts.Tenant, log)
	if err != nil {
		return nil, err
	}
	processor := orDefault[ContentProcessor](opts.Processor, stripProcessor{})
	rt := &Runtime{
		store:          newStore(opts.Pool, opts.Schema, opts.Tenant),
		schema:         opts.Schema,
		tenant:         opts.Tenant,
		userInfo:       opts.UserInfo,
		resolver:       opts.Resolver,
		media:          media,
		processor:      processor,
		moderator:      opts.Moderator,
		breaker:        newModeratorBreaker(opts.ModeratorTimeout, opts.ModeratorCooldown),
		classifier:     opts.Classifier,
		providerEraser: opts.ProviderDataEraser,
		anonymous:      opts.Anonymous,
		commentMax:     orDefault(opts.CommentMaxLength, DefaultCommentMaxLength),
		log:            log,
		cfg:            opts.Kinds,
		limiters:       limiters,
		held:           map[string]HeldSource{},
	}
	notices, err := notification.New(notification.Options{Pool: opts.Pool, Schema: opts.Schema, Tenant: opts.Tenant, Logger: log})
	if err != nil {
		return nil, err
	}
	rt.Notifications = notices
	rt.guard = &gate.Gate{Auth: opts.Auth, Log: log}
	rt.rules = newRules(rt)
	if err := rt.checkSchema(ctx); err != nil {
		return nil, err
	}
	// preferences before the writers that resolve through it; reactions before
	// comments, which reuse its applyTx primitive.
	rt.preferences = newPreferences(rt, opts.PreferenceSyncOverlap)
	rt.reactions = newReactions(rt)
	rt.polls = newPolls(rt)
	rt.comments = newComments(rt)
	rt.favorites = newFavorites(rt)
	return rt, nil
}

// checkSchema refuses a schema missing required ContentKit tables.
func (rt *Runtime) checkSchema(ctx context.Context) error {
	if _, err := rt.store.pool.Exec(ctx, `SELECT tenant_id, content_kind, content_id, content_version_id FROM `+rt.store.t.counts+` LIMIT 0;
		SELECT revision FROM `+rt.store.t.reactions+` LIMIT 0; SELECT value, revision FROM `+rt.store.t.favorites+` LIMIT 0;
		SELECT revision FROM `+rt.store.t.preferenceSync+` LIMIT 0;
		SELECT moderation FROM `+rt.store.t.comments+` LIMIT 0;
		SELECT kind, closes_at FROM `+rt.store.t.pollQuestions+` LIMIT 0; SELECT group_id FROM `+rt.store.t.pollAnswers+` LIMIT 0;
		SELECT scope, until FROM `+rt.store.t.commentBans+` LIMIT 0; SELECT language FROM `+rt.store.t.comments+` LIMIT 0;
		SELECT action FROM `+rt.store.t.bannedWords+` LIMIT 0; SELECT network FROM `+rt.store.t.ipBans+` LIMIT 0`); err != nil {
		return fmt.Errorf("content: schema %q lacks the ContentKit baseline (contentkit.New applies it): %w", rt.schema, err)
	}
	return nil
}

// Tenant returns the tenant this runtime is scoped to.
func (rt *Runtime) Tenant() string { return rt.tenant }

// Ref returns a reference to a work of this runtime's tenant.
func (rt *Runtime) Ref(contentKind, contentID string) contentref.ContentRef {
	return contentref.New(rt.tenant, contentKind, contentID)
}

// memoized runs each request in an access.WithMemo context.
func memoized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(access.WithMemo(r.Context())))
	})
}

// accessLog logs each request at DEBUG, and a 500 at ERROR with its cause.
func (rt *Runtime) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start)}
		if sw.status >= http.StatusInternalServerError {
			if sw.internalErr != nil {
				attrs = append(attrs, "err", sw.internalErr.Error())
			}
			rt.log.Error("content request failed", attrs...)
			return
		}
		rt.log.Debug("content request", attrs...)
	})
}

// --- shared helpers used by every module ---

// isRegistered reports a content kind or group kind interactions target.
func (rt *Runtime) isRegistered(kind string) bool {
	_, content := rt.cfg.Kinds[kind]
	_, group := rt.cfg.GroupKinds[kind]
	return content || group
}

// checkRef pins a caller-supplied reference to this tenant.
func (rt *Runtime) checkRef(ref contentref.ContentRef) error {
	if err := ref.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if ref.TenantID != rt.tenant {
		return ErrTenant
	}
	return nil
}

// canonicalRef merges the resolver's verdict into the requested reference: a
// zero Ref keeps the request, a foreign tenant is refused.
func (rt *Runtime) canonicalRef(requested, resolved contentref.ContentRef) (contentref.ContentRef, error) {
	if resolved.ContentID == "" {
		return requested, nil
	}
	if resolved.TenantID == "" {
		resolved.TenantID = rt.tenant
	}
	if resolved.ContentKind == "" {
		resolved.ContentKind = requested.ContentKind
	}
	if err := rt.checkRef(resolved); err != nil {
		return contentref.ContentRef{}, err
	}
	return resolved, nil
}

// gate resolves a target and enforces the required access level. The
// returned reference is the resolver's canonical identity (a merged group's
// survivor); callers store and query by it, never by the requested one.
func (rt *Runtime) gate(ctx context.Context, requested contentref.ContentRef, actor access.Actor, needAccessible bool) (contentref.ContentRef, error) {
	ref, _, err := rt.resolveTarget(ctx, requested, actor, needAccessible)
	return ref, err
}

// resolveTarget is gate that also returns the resolution (its Owner).
func (rt *Runtime) resolveTarget(ctx context.Context, requested contentref.ContentRef, actor access.Actor, needAccessible bool) (contentref.ContentRef, resolve.Resolution, error) {
	if !rt.routable(requested) {
		return contentref.ContentRef{}, resolve.Resolution{}, ErrNotFound
	}
	batch, err := rt.resolver.Resolve(ctx, []contentref.ContentRef{requested}, actor)
	if err != nil {
		return contentref.ContentRef{}, resolve.Resolution{}, err
	}
	ref, err := rt.admit(requested, batch, needAccessible)
	return ref, batch[requested.Key()], err
}

// admit applies requested's resolution from a batch: the canonical reference
// if the target exists and is visible (and accessible when required).
func (rt *Runtime) admit(requested contentref.ContentRef, batch map[contentref.ContentKey]resolve.Resolution, needAccessible bool) (contentref.ContentRef, error) {
	res, ok := batch[requested.Key()]
	if !ok {
		return contentref.ContentRef{}, ErrNotFound
	}
	ref, err := rt.canonicalRef(requested, res.Ref)
	if err != nil {
		return contentref.ContentRef{}, err
	}
	if !res.Visible {
		return contentref.ContentRef{}, ErrNotVisible
	}
	if needAccessible && !res.Accessible {
		return contentref.ContentRef{}, ErrForbidden
	}
	return ref, nil
}

func (rt *Runtime) routable(ref contentref.ContentRef) bool {
	return ref.TenantID == rt.tenant && rt.isRegistered(ref.ContentKind) && ref.ContentID != ""
}

// canonical maps a requested target to the resolver's canonical one for
// paths that must succeed even when the target is hidden (unfavoriting
// deleted content): a resolve failure keeps the requested one.
func (rt *Runtime) canonical(ctx context.Context, requested contentref.ContentRef, actor access.Actor) contentref.ContentRef {
	if rt.routable(requested) {
		if res, err := resolve.One(ctx, rt.resolver, requested, actor); err == nil {
			if ref, err := rt.canonicalRef(requested, res.Ref); err == nil {
				return ref
			}
		}
	}
	return requested
}

// requirePerm asks the actor's Verified whether it holds perm in the
// Authenticator's root scope. It fails closed: a denial is errForbidden, and
// a failed check an AuthError (401 when the credential ended, else 503).
func (rt *Runtime) requirePerm(ctx context.Context, actor access.Actor, perm access.Permission) error {
	ok, err := gate.Can(ctx, rt.guard.Auth, actor, string(perm))
	switch {
	case err != nil:
		return &AuthError{Err: err}
	case !ok:
		return errForbidden
	}
	return nil
}

// actor is the request's actor as the route gate built it; anonymous outside
// a request.
func (rt *Runtime) actor(ctx context.Context) access.Actor { return gate.Actor(ctx) }

// requireActor demands a signed-in person: 401 when anonymous, 403 for an
// application.
func (rt *Runtime) requireActor(ctx context.Context) (access.Actor, error) {
	a := gate.Actor(ctx)
	if err := participant(a, false); err != nil {
		return access.Actor{}, err
	}
	return a, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
