// Package versions binds media to content versions. A content kind's media
// items are its versions, {kind, content, version}, and one content-level
// item per content; this package answers media's hooks from the content
// model (who uploads, who reads, the facts a version's media yields),
// publishes versions to viewers (segment items, previews, playback), forks
// and imports versions' media, and serves the reader plan, reader metadata
// and downloads.
//
// The catalog's visibility, the gate's access and the listing projection are
// other modules' decisions, reached through Visibility, Access and Facets.
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
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Target names a content (Version "") or one of its versions.
type Target struct {
	Content string `json:"content"`
	Version string `json:"version,omitempty"`
}

// Visibility is the catalog's decision for one viewer.
type Visibility interface {
	// See answers per target; one it omits is unseen.
	See(ctx context.Context, actor access.Actor, targets []Target) (map[Target]Sight, error)
}

// Sight is what a viewer sees of a content or a version.
type Sight struct {
	Visible bool // live, or the viewer edits it
	Live    bool // live for everyone: published, due and not deleted
	Editor  bool // staff, a creator or an owner-group editor
}

// Access is the gate's decision for one viewer.
type Access interface {
	// Open answers per version; one it omits is closed.
	Open(ctx context.Context, actor access.Actor, versions []Target) (map[Target]Opening, error)
	// Tiers are the site's tiers actor holds.
	Tiers(ctx context.Context, actor access.Actor) ([]string, error)
}

// Opening is what a viewer opens of a version.
type Opening struct {
	Opens bool // every released segment
	Free  int  // leading segments open without Opens: the rule's free preview
}

// Facets refreshes the listing projection of versions whose derived media
// facts changed, in the writing transaction.
type Facets interface {
	RefreshTx(ctx context.Context, tx pgx.Tx, versions []string) error
}

// Others answer the media kinds that are not content: group covers, posts'
// and polls' images.
type Others interface {
	access.ContentResolver
	media.UploadAuthorizer
}

// Options configure a Service.
type Options struct {
	Tenant     string // the content tables' tenant
	Schema     string // ContentKit's schema
	Pool       *pgxpool.Pool
	Visibility Visibility
	Access     Access
	Facets     Facets
	Others     Others      // nil refuses every other kind
	Watermark  Watermarker // nil serves prepared archives
	Downloads  Downloads
	Logger     *slog.Logger
	Now        func() time.Time
}

// Media is the media runtime built over Service.Hooks.
type Media struct {
	Manifests *media.Manifests
	Uploads   *media.Uploads
	Jobs      *media.Jobs
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
	items, versions, segments, publications, pubItems string
}

// New validates o. Build the media runtime with Hooks, then Bind it.
func New(o Options) (*Service, error) {
	if o.Tenant == "" || o.Schema == "" || o.Pool == nil || o.Visibility == nil || o.Access == nil || o.Facets == nil {
		return nil, errors.New("versions: Tenant, Schema, Pool, Visibility, Access and Facets are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	q := func(t string) string { return pgxIdent(o.Schema, t) }
	s := &Service{o: o, t: tables{items: q("content_items"), versions: q("content_versions"), segments: q("content_segments"),
		publications: q("content_version_publications"), pubItems: q("content_version_publication_items")}}
	var err error
	if s.dl, err = newQuotas(o.Downloads, o.Logger, o.Now); err != nil {
		return nil, err
	}
	return s, nil
}

// Hooks are media's hooks over the content model; set them as
// media.Config.Hooks (with the host's PurgePublic) before building the
// registry.
func (s *Service) Hooks() media.Hooks {
	return media.Hooks{Resolver: resolver{s}, CanUpload: uploads{s}, ItemReady: s.itemReady, ItemCommitted: s.itemCommitted, Failed: s.failed}
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

// kind is a version's media kind with Publish, or an error.
func (s *Service) published(v version) (*media.Kind, error) {
	k, err := s.reg.Kind(v.Kind)
	if err != nil || k.VersionKind() == nil || k.VersionKind().Publish == nil {
		return nil, fmt.Errorf("%w: kind %q publishes no versions", ErrNotFound, v.Kind)
	}
	return k.VersionKind(), nil
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
	// ErrForbidden is what the viewer sees but may not do.
	ErrForbidden = errors.New("versions: forbidden")
	// ErrInvalid is a malformed request.
	ErrInvalid = errors.New("versions: invalid")
	// errRetired is a publication an edit superseded while it was built.
	errRetired = errors.New("versions: publication retired")
)

func pgxIdent(schema, table string) string { return pgx.Identifier{schema, table}.Sanitize() }
