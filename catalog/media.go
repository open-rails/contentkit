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
// item (kind, content, version) holds its cover, and its body item ({kind}.body,
// content, version) its copy of the body's images, shown only to viewers who
// open the version.
type Media struct {
	Images Images // *media.Manifests
	// Reader gives editors an unpublished item's image previews.
	Reader   Reader
	Versions VersionMedia // *versions.Service
}

// Images answers image queries from current publications in one read.
type Images interface {
	Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error)
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
}

var errMediaNotConfigured = errors.New("catalog: media is not bound")

// BindMedia attaches media; call it once, before serving.
func (s *Store) BindMedia(m Media) error {
	if m.Images == nil || m.Versions == nil {
		return errors.New("catalog: Media needs Images and Versions")
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

// imageSet resolves the images one response shows with one lookup.
type imageSet struct {
	s    *Store
	keys []imageKey
	urls map[imageKey]string
}

type imageKey struct{ kind, id, version, name string }

func (s *Store) imageSet() *imageSet { return &imageSet{s: s, urls: map[imageKey]string{}} }

func (set *imageSet) add(k imageKey) {
	if k.name != "" {
		set.keys = append(set.keys, k)
	}
}

func (set *imageSet) resolve(ctx context.Context) error {
	if set.s.media == nil || len(set.keys) == 0 {
		return nil
	}
	qs := make([]media.ImageQuery, len(set.keys))
	for i, k := range set.keys {
		qs[i] = media.ImageQuery{Ref: set.s.Ref(k.kind, k.id).WithVersion(k.version), Name: k.name}
	}
	found, err := set.s.media.Images.Images(ctx, qs...)
	if err != nil {
		return err
	}
	for i, k := range set.keys {
		if len(found[i]) > 0 && len(found[i][0].Renditions) > 0 {
			set.urls[k] = found[i][0].URL(math.MaxInt)
		}
	}
	return nil
}

func (set *imageSet) url(k imageKey) (string, bool) {
	u, ok := set.urls[k]
	return u, ok
}

// BodyImageInput names a body image uploaded to one version's body item.
type BodyImageInput struct {
	Image   *string `json:"image"`
	Version string  `json:"version"`
}

// BodyImage takes a body image an editor uploaded to a version's body item
// ({kind}.body): it queues the image's copy into every other version's and
// returns its reference and a URL that shows it to the editor now.
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
	url, err := s.previewURL(ctx, actor, imageKey{got[contentID].kind + media.BodySuffix, contentID, in.Version, *in.Image})
	if err != nil {
		return InlineImage{}, err
	}
	out := InlineImage{Ref: ImageRef(*in.Image)}
	if url != "" {
		out.URL = &url
	}
	return out, nil
}

// previewURL shows an editor an image of a version's body item now: its
// public file, else its editor view once rendered; "" while neither exists.
func (s *Store) previewURL(ctx context.Context, editor access.Actor, k imageKey) (string, error) {
	set := s.imageSet()
	set.add(k)
	if err := set.resolve(ctx); err != nil {
		return "", err
	}
	if u, ok := set.url(k); ok {
		return u, nil
	}
	if s.media.Reader == nil {
		return "", nil
	}
	read, err := s.media.Reader.Read(ctx, s.Ref(k.kind, k.id).WithVersion(k.version), editor, media.ReadOptions{Editor: true, Limit: math.MaxInt})
	if err != nil {
		return "", err
	}
	for _, f := range read.Files {
		if f.Upload && strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path)) == k.name {
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
