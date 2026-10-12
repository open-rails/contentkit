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
)

// Media connects content items to ContentKit media. A content item's folder
// is addressed by its reference (kind, content); each version's by
// (kind, content, version). Covers and body images are inline images of the
// content's folder: the browser uploads one to a named upload path and hands
// its name ("i-{uuid}") to the catalog.
type Media struct {
	Images Images  // *media.Manifests
	Expose Exposer // *media.Jobs
	// Reader gives editors an unpublished item's image previews.
	Reader Reader
}

// Images answers image queries from current publications in one read.
type Images interface {
	Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error)
}

// Exposer queues visibility changes and folder deletions in the writing
// transaction.
type Exposer interface {
	ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error
	DeleteItemsTx(ctx context.Context, tx pgx.Tx, items ...media.Deletion) error
}

// Reader reads an item as the media read API does.
type Reader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
}

var errMediaNotConfigured = errors.New("catalog: Options.Media is not configured")

func newMedia(m *Media) (*Media, error) {
	if m.Images == nil || m.Expose == nil {
		return nil, errors.New("catalog: Media needs Images and Expose")
	}
	out := *m
	return &out, nil
}

// folders are the media folders of content items: each content's and each of
// its versions'.
func (s *Store) folders(ctx context.Context, q Querier, ids []string) ([]contentref.ContentRef, error) {
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, coalesce(v.id, '') FROM `+s.t.items+` c
 LEFT JOIN `+s.t.versions+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id
 WHERE c.tenant_id = $1 AND c.id = ANY($2) ORDER BY c.id, v.id`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentref.ContentRef
	seen := map[string]bool{}
	for rows.Next() {
		var kind, id, version string
		if err := rows.Scan(&kind, &id, &version); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, s.Ref(kind, id))
		}
		if version != "" {
			out = append(out, contentref.NewVersion(s.tenant, kind, id, version))
		}
	}
	return out, rows.Err()
}

// exposeTx asks media to bring the items' public files to their visibility.
func (s *Store) exposeTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	if s.media == nil || len(ids) == 0 {
		return nil
	}
	refs, err := s.folders(ctx, tx, ids)
	if err != nil || len(refs) == 0 {
		return err
	}
	return s.media.Expose.ExposeTx(ctx, tx, refs...)
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

// CanUpload is media's UploadAuthorizer for content folders: the actor edits
// the content item (or, for a version folder, its content), which exists.
// Other targets are refused.
func (s *Store) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	ref := t.Ref
	if ref.TenantID != s.tenant {
		return media.UploadGrant{}, nil
	}
	if _, ok := s.cfg.Kinds[ref.ContentKind]; !ok {
		return media.UploadGrant{}, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return media.UploadGrant{}, err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, []string{ref.ContentID})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return media.UploadGrant{}, nil
	case err != nil:
		return media.UploadGrant{}, err
	}
	o := got[ref.ContentID]
	if o.kind != ref.ContentKind || o.state == StateDeleted {
		return media.UploadGrant{}, nil
	}
	if v := ref.Version(); v != "" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND id = $3 AND state <> 'deleted')`,
			s.tenant, ref.ContentID, v).Scan(&ok); err != nil || !ok {
			return media.UploadGrant{}, err
		}
	}
	return media.UploadGrant{Allowed: true}, nil
}

var _ media.UploadAuthorizer = (*Store)(nil)
