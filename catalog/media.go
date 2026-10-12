package catalog

import (
	"context"
	"errors"
	"math"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// Media connects content items to ContentKit media, built over the
// catalog's Resolver and RefreshTx (media/versions): bind it with BindMedia
// before serving. A content item's media item is (kind, content), holding its
// cover and body images; each version's is (kind, content, version).
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
}

// VersionMedia is the media calls catalog writes make in their transaction:
// a target is a content item (its item and every version's) or one version.
type VersionMedia interface {
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

type imageKey struct{ kind, id, name string }

func (s *Store) imageSet() *imageSet { return &imageSet{s: s, urls: map[imageKey]string{}} }

func (set *imageSet) add(kind, id, name string) {
	k := imageKey{kind, id, name}
	if name != "" {
		set.keys = append(set.keys, k)
	}
}

func (set *imageSet) resolve(ctx context.Context) error {
	if set.s.media == nil || len(set.keys) == 0 {
		return nil
	}
	qs := make([]media.ImageQuery, len(set.keys))
	for i, k := range set.keys {
		qs[i] = media.ImageQuery{Ref: set.s.Ref(k.kind, k.id), Name: k.name}
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

func (set *imageSet) url(kind, id, name string) (string, bool) {
	u, ok := set.urls[imageKey{kind, id, name}]
	return u, ok
}

// previewURL shows an editor an image of the content's folder now: its public
// file, else its editor view once rendered; "" while neither exists.
func (s *Store) previewURL(ctx context.Context, editor access.Actor, kind, id, name string) (string, error) {
	set := s.imageSet()
	set.add(kind, id, name)
	if err := set.resolve(ctx); err != nil {
		return "", err
	}
	if u, ok := set.url(kind, id, name); ok {
		return u, nil
	}
	if s.media.Reader == nil {
		return "", nil
	}
	read, err := s.media.Reader.Read(ctx, s.Ref(kind, id), editor, media.ReadOptions{Editor: true, Limit: math.MaxInt})
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
