// Package versions binds media to content versions. A content kind's media
// items are its versions, {kind, content, version}; a content has none of
// its own. This package answers media's
// hooks from the content model (who uploads, who reads, the facts a
// version's media yields), records immutable ready-file snapshots for viewers
// (chapter metadata, signed previews and playback), forks and imports versions' media, copies body
// images between versions, and serves the reader plan, reader metadata and
// downloads.
//
// Whether a viewer sees, opens or edits a content or a version is the
// catalog's decision over the access gate, ContentKit's own
// resolve.Resolver; this package adds segments' release and the rule's
// preview to it. The listing projection is refreshed through Facets.
package versions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// Target names a content (Version "") or one of its versions.
type Target struct {
	Content string `json:"content"`
	Version string `json:"version,omitempty"`
}

// Facets refreshes the listing projection of versions whose derived media
// facts changed, in the writing transaction.
type Facets interface {
	RefreshTx(ctx context.Context, tx pgx.Tx, versions []string) error
}

// Others answer the media kinds of neither contents nor groups: posts' and
// polls' images.
type Others interface {
	resolve.Resolver
	media.UploadAuthorizer
}

// Options configure a Service.
type Options struct {
	Tenant string // the content tables' tenant
	Schema string // ContentKit's schema
	Pool   *pgxpool.Pool
	Kinds  kinds.Config       // the site's content and group kinds
	Auth   auth.Authenticator // the host's: staff and group scope checks
	// AccountExists verifies avatar targets in this tenant's account directory.
	// Scoped engines install it; nil preserves the legacy host contract.
	AccountExists func(context.Context, string) (bool, error)
	// Resolver decides versions ({kind, content, version}) for a viewer:
	// Visible (live, or edited by them), Accessible (opened by them) and
	// Editor.
	Resolver resolve.Resolver
	// Gate answers the tiers a viewer holds for download quotas; optional.
	Gate      *access.Gate
	Facets    Facets
	Others    Others         // nil refuses every other kind
	Policy    DownloadPolicy // the host's download policy; nil allows every download and marks none
	Downloads Downloads
	Logger    *slog.Logger
	Now       func() time.Time
}

// Media is the media runtime built over Service.Hooks.
type Media struct {
	Manifests *media.Manifests
	Uploads   *media.Uploads
	Jobs      *media.Jobs
	Reader    *media.Reader // signs file downloads; nil serves none
}

// Service is media for content versions.
type Service struct {
	o   Options
	t   tables
	reg *media.Registry
	m   Media
	dl  *quotas

	mu     sync.Mutex
	client *river.Client[pgx.Tx]
}

type tables struct {
	items, versions, segments, publications, groups, facets string
}

// New validates o. Build the media runtime with Hooks, then Bind it.
func New(o Options) (*Service, error) {
	if o.Tenant == "" || o.Schema == "" || o.Pool == nil || o.Resolver == nil || o.Facets == nil {
		return nil, errors.New("versions: Tenant, Schema, Pool, Resolver and Facets are required")
	}
	if err := o.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("versions: Kinds: %w", err)
	}
	if err := gate.Check(o.Auth); err != nil {
		return nil, fmt.Errorf("versions: Auth: %w", err)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	q := func(t string) string { return pgxIdent(o.Schema, t) }
	s := &Service{o: o, t: tables{items: q("content_items"), versions: q("content_versions"), segments: q("content_segments"),
		publications: q("content_version_publications"), groups: q("content_groups"), facets: q("content_facets")}}
	var err error
	if s.dl, err = newQuotas(o.Downloads, o.Kinds.Tiers, o.Logger, o.Now); err != nil {
		return nil, err
	}
	return s, nil
}

// Hooks are media's hooks over the content model; set them as
// media.Config.Hooks (with the host's PurgePublic) before building the
// registry.
func (s *Service) Hooks() media.Hooks {
	return media.Hooks{Resolver: resolver{s}, CanUpload: uploads{s}, ItemReady: s.itemReady, ItemProgressed: s.itemProgressed, ItemCommitted: s.itemCommitted, Failed: s.failed}
}

// Bind attaches the media runtime built over Hooks.
func (s *Service) Bind(m Media) error {
	if m.Manifests == nil || m.Uploads == nil || m.Jobs == nil {
		return errors.New("versions: Bind needs Manifests, Uploads and Jobs")
	}
	s.m, s.reg = m, m.Manifests.Registry()
	return nil
}

// system is this package's own actor: its commits and ingests. No
// authenticator yields its kind.
var system = access.Actor{ID: "contentkit", Kind: "contentkit:system"}

func isSystem(a access.Actor) bool { return a.Kind == system.Kind && a.ID == system.ID && !a.Anonymous }

// ref is the media ref of a target of kind.
func (s *Service) ref(kind string, t Target) (contentref.ContentRef, error) {
	return s.reg.VersionRef(kind, t.Content, t.Version)
}

// decide resolves targets of kind for actor.
func (s *Service) decide(ctx context.Context, actor access.Actor, kind string, targets []Target) (map[Target]resolve.Resolution, error) {
	refs := make([]contentref.ContentRef, len(targets))
	for i, t := range targets {
		refs[i] = contentref.New(s.o.Tenant, kind, t.Content).WithVersion(t.Version)
	}
	got, err := s.o.Resolver.Resolve(ctx, refs, actor)
	if err != nil {
		return nil, err
	}
	out := make(map[Target]resolve.Resolution, len(targets))
	for i, t := range targets {
		out[t] = got[refs[i].Key()]
	}
	return out, nil
}

// previews are versions' rules' free parts (access.Rule.Preview), else
// their kind's locked preview.
func (s *Service) previews(ctx context.Context, q querier, versions []string) (map[string]access.Preview, error) {
	rows, err := q.Query(ctx, `SELECT v.id, c.kind, c.access, v.access FROM `+s.t.versions+` v
JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id WHERE v.tenant_id = $1 AND v.id = ANY($2)`, s.o.Tenant, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]access.Preview{}
	for rows.Next() {
		var id, kind string
		var content access.Rule
		var override *access.Rule
		if err := rows.Scan(&id, &kind, &content, &override); err != nil {
			return nil, err
		}
		if p := access.Effective(content, override).Previewed(s.o.Kinds.Kinds[kind]).Preview; p != nil {
			out[id] = *p
		}
	}
	return out, rows.Err()
}

// version is one version row and its content's kind.
type version struct {
	Target
	Kind     string
	Language string
}

// versions loads the versions of targets that exist, keyed by target.
func (s *Service) versions(ctx context.Context, q querier, targets []Target) (map[Target]version, error) {
	contents, ids := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], ids[i] = t.Content, t.Version
	}
	rows, err := q.Query(ctx, `SELECT v.content_id, v.id, c.kind, v.languages[1] FROM unnest($2::text[], $3::text[]) AS t(content, version)
JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.id = t.version AND v.content_id = t.content
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = v.content_id`, s.o.Tenant, contents, ids)
	if err != nil {
		return nil, err
	}
	out := map[Target]version{}
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.Content, &v.Version, &v.Kind, &v.Language); err != nil {
			rows.Close()
			return nil, err
		}
		out[v.Target] = v
	}
	return out, rows.Err()
}

// one loads one version.
func (s *Service) one(ctx context.Context, t Target) (version, error) {
	vs, err := s.versions(ctx, s.o.Pool, []Target{t})
	if err != nil {
		return version{}, err
	}
	v, ok := vs[t]
	if !ok {
		return version{}, ErrNotFound
	}
	return v, nil
}

// published is a version's kind, which must publish.
func (s *Service) published(v version) (*media.Kind, error) {
	k, err := s.reg.Kind(v.Kind)
	if err != nil || !k.Versions || k.Publish == nil {
		return nil, fmt.Errorf("%w: kind %q publishes no versions", ErrNotFound, v.Kind)
	}
	return k, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	// ErrNotFound is a content or version that does not exist, or that the
	// viewer may not see.
	ErrNotFound = errors.New("versions: not found")
	// ErrNotReady is a version whose media is still processing, being
	// forked or imported, or not yet published.
	ErrNotReady = errors.New("versions: media not ready")
	// ErrNoDownload is a version without the file its kind's download
	// serves: nothing uploaded, or an upload that failed or was dropped.
	ErrNoDownload = errors.New("versions: no download")
	// ErrForbidden is what the viewer sees but may not do.
	ErrForbidden = errors.New("versions: forbidden")
	// ErrInvalid is a malformed request.
	ErrInvalid = errors.New("versions: invalid")
	// errRetired is a publication an edit superseded while it was built.
	errRetired = errors.New("versions: publication retired")
)

func pgxIdent(schema, table string) string { return pgx.Identifier{schema, table}.Sanitize() }
