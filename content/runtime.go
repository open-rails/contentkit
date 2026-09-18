package content

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

// Reserved content kinds owned by this package: reactions on comments and
// posts are keyed by them and never pass through the ContentResolver.
const (
	KindComment = "comment"
	KindPost    = "post"
)

// Options configures a Runtime. Pool, Schema, Tenant, Identity, Authz and
// Resolver are mandatory; the rest fall back to documented defaults.
type Options struct {
	// Pool is the host's shared pgx pool; the runtime does not own its lifecycle.
	Pool *pgxpool.Pool
	// Schema is the host schema holding the social_* tables (contentkit.Migrate).
	Schema string
	// Tenant scopes every row, index, cursor and result. Required.
	Tenant string
	// SearchSchema is the keyword-profile schema whose dirty queue receives
	// posts as search documents. Empty: posts are not indexed.
	SearchSchema string

	// Mandatory ports.
	Identity Identity
	Authz    Authorizer
	Resolver ContentResolver

	// Canonicalizer enables the preference boundary (preferences.go): the
	// reaction/favorite row, the counts rollup and the exported snapshot all
	// use the reference it returns. nil = no preference export.
	Canonicalizer ContentCanonicalizer

	// Optional ports (nil -> default).
	Users     UserEnricher     // default: no enrichment (ids only)
	Media     MediaStore       // explicit override; usually leave nil and set Storage
	Processor ContentProcessor // default: strip tags

	// Storage configures the built-in S3-backed media store (poll/post image
	// upload to a public bucket); used when Media is nil. See StorageConfig.
	Storage *StorageConfig

	// Perms are the opaque host permission strings gating privileged writes.
	Perms Perms

	// ContentKinds are the commentable/reactable/favoritable kinds the host
	// registers (e.g. "gallery", "video", "post"). Unregistered kinds are 404.
	ContentKinds []string

	// Logger receives the access log: each request at DEBUG, a 500 at ERROR
	// with its cause. nil -> slog.Default().
	Logger *slog.Logger
}

// Runtime is one tenant's embedded content module: shared deps + the module
// services, exposing one mountable http.Handler.
type Runtime struct {
	store        *store
	schema       string
	tenant       string
	searchSchema string
	identity     Identity
	authz        Authorizer
	resolver     ContentResolver
	users        UserEnricher
	media        MediaStore
	processor    ContentProcessor
	perms        Perms
	log          *slog.Logger
	kinds        map[string]struct{}
	// mediaBase absolutizes stored relative media paths (backfilled rows)
	// against the public bucket origin; empty = serve values verbatim.
	mediaBase string

	preferences *preferences
	reactions   *reactions
	polls       *polls
	comments    *comments
	posts       *posts
	favorites   *favorites
}

// New constructs a Runtime over a schema the social lineage was applied to
// (Migrate) and wires the module services.
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
	if opts.Identity == nil || opts.Authz == nil || opts.Resolver == nil {
		return nil, fmt.Errorf("content: Identity, Authz and Resolver ports are required")
	}
	media, err := resolveMedia(opts)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		store:        newStore(opts.Pool, opts.Schema, opts.Tenant),
		schema:       opts.Schema,
		tenant:       opts.Tenant,
		searchSchema: strings.TrimSpace(opts.SearchSchema),
		identity:     opts.Identity,
		authz:        opts.Authz,
		resolver:     opts.Resolver,
		users:        orDefault[UserEnricher](opts.Users, noopEnricher{}),
		media:        media,
		processor:    orDefault[ContentProcessor](opts.Processor, stripProcessor{}),
		perms:        opts.Perms,
		log:          orDefault[*slog.Logger](opts.Logger, slog.Default()),
		kinds:        make(map[string]struct{}, len(opts.ContentKinds)),
	}
	if opts.Storage != nil {
		rt.mediaBase = strings.TrimRight(opts.Storage.PublicBaseURL, "/")
	}
	for _, k := range opts.ContentKinds {
		rt.kinds[k] = struct{}{}
	}
	if err := rt.checkSchema(ctx); err != nil {
		return nil, err
	}
	// preferences before the writers that record into it; reactions before
	// comments and posts, which reuse its applyTx primitive.
	rt.preferences = newPreferences(rt, opts.Canonicalizer)
	rt.reactions = newReactions(rt)
	rt.polls = newPolls(rt)
	rt.comments = newComments(rt)
	rt.posts = newPosts(rt)
	rt.favorites = newFavorites(rt)
	return rt, nil
}

// checkSchema fails construction when the social lineage (incl. the content
// reference migration) is not applied to the schema.
func (rt *Runtime) checkSchema(ctx context.Context) error {
	if _, err := rt.store.pool.Exec(ctx, `SELECT tenant_id, content_kind, content_id, content_version_id FROM `+rt.store.t.counts+` LIMIT 0; SELECT revision FROM `+rt.store.t.preferenceSnapshots+` LIMIT 0`); err != nil {
		return fmt.Errorf("content: schema %q lacks the social lineage (apply contentkit.Migrate): %w", rt.schema, err)
	}
	return nil
}

// Tenant returns the tenant this runtime is scoped to.
func (rt *Runtime) Tenant() string { return rt.tenant }

// Ref returns a reference to a work of this runtime's tenant.
func (rt *Runtime) Ref(contentKind, contentID string) contentref.ContentRef {
	return contentref.New(rt.tenant, contentKind, contentID)
}

// Handler returns the mountable http.Handler. The host mounts it under a prefix
// (e.g. "/api/social/") after its own auth middleware populated the identity.
func (rt *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	rt.reactions.mount(mux)
	rt.polls.mount(mux)
	rt.comments.mount(mux)
	rt.posts.mount(mux)
	rt.favorites.mount(mux)
	return rt.accessLog(mux)
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

var contentKindRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func (rt *Runtime) isRegistered(kind string) bool {
	_, ok := rt.kinds[kind]
	return ok
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

// gate resolves a route target and enforces the required access level. The
// returned reference is the resolver's canonical identity; callers store and
// query by it, never by the caller-supplied key, so aliases cannot fragment rows.
func (rt *Runtime) gate(ctx context.Context, kind, id string, actor Actor, needAccessible bool) (contentref.ContentRef, error) {
	if !contentKindRe.MatchString(kind) || !rt.isRegistered(kind) || id == "" {
		return contentref.ContentRef{}, ErrNotFound
	}
	requested := rt.Ref(kind, id)
	res, err := rt.resolver.Resolve(ctx, requested, actor)
	if err != nil {
		return contentref.ContentRef{}, err
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

// canonical maps a caller-supplied key to the resolver's canonical one for
// paths that must succeed even when the target is hidden (un-wishlisting
// deleted content): a resolve failure falls back to the raw key.
func (rt *Runtime) canonical(ctx context.Context, kind, id string, actor Actor) contentref.ContentRef {
	requested := rt.Ref(kind, id)
	if !contentKindRe.MatchString(kind) || !rt.isRegistered(kind) || id == "" {
		return requested
	}
	res, err := rt.resolver.Resolve(ctx, requested, actor)
	if err != nil {
		return requested
	}
	ref, err := rt.canonicalRef(requested, res.Ref)
	if err != nil {
		return requested
	}
	return ref
}

// requirePerm is fail-closed: an unset perm, a denied check, or a check error
// all deny.
func (rt *Runtime) requirePerm(ctx context.Context, actor Actor, perm string) error {
	if perm == "" {
		return errForbidden
	}
	ok, err := rt.authz.Can(ctx, actor, perm)
	if err != nil || !ok {
		return errForbidden
	}
	return nil
}

// absMediaURL absolutizes a stored relative media path against the public
// bucket origin; absolute URLs and unset values pass through.
func (rt *Runtime) absMediaURL(u string) string {
	if u == "" || rt.mediaBase == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return rt.mediaBase + "/" + strings.TrimLeft(u, "/")
}

// actor reads the (possibly anonymous) authenticated actor from context.
func (rt *Runtime) actor(ctx context.Context) Actor {
	a, ok := rt.identity.Actor(ctx)
	if !ok {
		return Actor{Anonymous: true}
	}
	return a
}

// requireActor demands a non-anonymous authenticated actor.
func (rt *Runtime) requireActor(ctx context.Context) (Actor, error) {
	a, ok := rt.identity.Actor(ctx)
	if !ok || a.Anonymous || a.ID == "" {
		return Actor{}, errUnauthorized
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
