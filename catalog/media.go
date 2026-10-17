package catalog

import (
	"context"
	"errors"
	"math"
	"path"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// Media connects content items to ContentKit media, built over the
// catalog's Resolver and RefreshTx (media/versions): bind it with BindMedia
// before serving. A content's media lives in its versions: each version's
// item (kind, content, version) holds its cover and, in private/, its copy of
// the body's images, which only the version's openers read.
type Media struct {
	// Registry constructs media references in each kind's effective namespace.
	// Nil retains the legacy standalone binding to the catalog key.
	Registry *media.Registry
	// Reader signs opened versions' body images and gives editors previews.
	Reader   Reader
	Versions VersionMedia // *versions.Service
}

// Reader reads an item as the media read API does.
type Reader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
	// NamedURLs signs an opened item's named images that have no public files.
	NamedURLs(ctx context.Context, ref contentref.ContentRef, actor access.Actor, names []string) (map[string]string, error)
}

// VersionMedia is the media calls catalog writes make in their transaction
// (a target is a content item, every version's media, or one version) and
// reads make for covers.
type VersionMedia interface {
	// Covers resolves versions' covers.
	Covers(ctx context.Context, covers []versions.CoverOf) ([][]media.PublicRendition, error)
	// CopyBodiesTx copies body images between a content's versions.
	CopyBodiesTx(ctx context.Context, tx pgx.Tx, copies []versions.BodyCopy) error
	// ExposeTx follows a visibility change.
	ExposeTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error
	// RepublishTx follows a change of a version's effective rule or segments.
	RepublishTx(ctx context.Context, tx pgx.Tx, versions []versions.Target) error
	// DeleteTx deletes media before the rows go.
	DeleteTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error
	// ForkTx fills new versions' media from siblings.
	ForkTx(ctx context.Context, tx pgx.Tx, forks []versions.Fork) error
	// PendingTx reports versions whose media is not ready to go live.
	PendingTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) ([]versions.MediaPending, error)
}

var errMediaNotConfigured = errors.New("catalog: media is not bound")

// NotReadyError refuses making versions live while their media is still
// processing or being published; it is versions.ErrNotReady.
type NotReadyError struct{ Pending []versions.MediaPending }

func (e *NotReadyError) Error() string {
	reasons := make([]string, len(e.Pending))
	for i, p := range e.Pending {
		reasons[i] = p.Reason
	}
	slices.Sort(reasons)
	return "media not ready: " + strings.Join(slices.Compact(reasons), "; ")
}

func (e *NotReadyError) Unwrap() error { return versions.ErrNotReady }

// readyTx refuses targets that would go live without readable media.
func (s *Store) readyTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if s.media == nil || len(targets) == 0 {
		return nil
	}
	pending, err := s.media.Versions.PendingTx(ctx, tx, targets)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return &NotReadyError{Pending: pending}
	}
	return nil
}

// BindMedia attaches media; call it once, before serving.
func (s *Store) BindMedia(m Media) error {
	if m.Versions == nil {
		return errors.New("catalog: Media needs Versions")
	}
	s.media = &m
	return nil
}

// RefreshTx is versions.Facets: it refreshes the facets of the versions'
// content items after their media facts changed.
func (s *Store) RefreshTx(ctx context.Context, tx pgx.Tx, versionIDs []string) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT content_id FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, versionIDs)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return s.RefreshFacets(ctx, tx, ids)
}

func contentTargets(ids []string) []versions.Target {
	out := make([]versions.Target, len(ids))
	for i, id := range ids {
		out[i] = versions.Target{Content: id}
	}
	return out
}

// exposeTx asks media to bring the items' media to their visibility.
func (s *Store) exposeTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	if s.media == nil || len(ids) == 0 {
		return nil
	}
	return s.media.Versions.ExposeTx(ctx, tx, contentTargets(ids))
}

// BodyImageInput names a body image uploaded to one version (Upload.Body).
type BodyImageInput struct {
	Image   *string `json:"image"`
	Version string  `json:"version"`
}

// BodyImage takes a body image an editor uploaded to a version (Upload.Body):
// it queues the image's copy into every other version and returns its
// reference and a URL that shows it to the editor now.
func (s *Store) BodyImage(ctx context.Context, actor access.Actor, contentID string, in BodyImageInput) (InlineImage, error) {
	if in.Image == nil || !media.ValidNamed(*in.Image) {
		return InlineImage{}, invalid("image must be an inline image name (i-{uuid})")
	}
	if s.media == nil {
		return InlineImage{}, errMediaNotConfigured
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return InlineImage{}, err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, []string{contentID})
	if err == nil && got[contentID].state == StateDeleted {
		err = ErrNotFound
	}
	if err != nil {
		return InlineImage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND state <> 'deleted'`, s.tenant, contentID)
	if err != nil {
		return InlineImage{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return InlineImage{}, err
	}
	if !slices.Contains(ids, in.Version) {
		return InlineImage{}, invalid("version %q is not a version of %s", in.Version, contentID)
	}
	if err := s.media.Versions.CopyBodiesTx(ctx, tx, []versions.BodyCopy{{Content: contentID, From: in.Version, To: ids, Names: []string{*in.Image}}}); err != nil {
		return InlineImage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return InlineImage{}, err
	}
	ref, err := s.mediaRef(got[contentID].kind, contentID, in.Version)
	if err != nil {
		return InlineImage{}, err
	}
	url, err := s.previewURL(ctx, actor, ref, *in.Image)
	if err != nil {
		return InlineImage{}, err
	}
	out := InlineImage{Ref: ImageRef(*in.Image)}
	if url != "" {
		out.URL = &url
	}
	return out, nil
}

// mediaRef keeps media partitions separate from catalog and signal refs.
func (s *Store) mediaRef(kind, id, version string) (contentref.ContentRef, error) {
	if s.media != nil && s.media.Registry != nil {
		return s.media.Registry.VersionRef(kind, id, version)
	}
	return s.Ref(kind, id).WithVersion(version), nil
}

// previewURL shows an editor an upload of an item now, its editor view once
// rendered; "" until then.
func (s *Store) previewURL(ctx context.Context, editor access.Actor, ref contentref.ContentRef, name string) (string, error) {
	if s.media.Reader == nil {
		return "", nil
	}
	read, err := s.media.Reader.Read(ctx, ref, editor, media.ReadOptions{Editor: true, Limit: math.MaxInt})
	if err != nil {
		return "", err
	}
	for _, f := range read.Files {
		if f.Upload && strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path)) == name {
			return f.EditorURL, nil
		}
	}
	return "", nil
}

// coverURL is the URL of rs's widest rendition.
func coverURL(rs []media.PublicRendition) *string {
	if len(rs) == 0 {
		return nil
	}
	u := media.PublicImage{Renditions: rs}.URL(math.MaxInt)
	return &u
}

// Existing reports which of ids are this tenant's content items or versions,
// for the media orphan sweep; deleted rows not yet purged still exist.
func (s *Store) Existing(ctx context.Context, ids []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)
 UNION SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// publishedVersions are the items' published versions, which go live with them.
func (s *Store) publishedVersions(ctx context.Context, tx pgx.Tx, ids []string) ([]versions.Target, error) {
	if s.media == nil || len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT content_id, id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = ANY($2) AND state = 'published'`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[versions.Target])
}
