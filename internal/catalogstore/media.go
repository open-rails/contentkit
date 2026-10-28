package catalogstore

import (
	"context"
	"errors"
	"math"
	"path"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
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
	Registry *mediastore.Registry
	// Reader signs opened versions' body images and gives editors previews.
	Reader   catalog.Reader
	Versions VersionMedia // *versions.Service
}

// VersionMedia is the media calls catalog writes make in their transaction
// (a target is a content item, every version's media, or one version) and
// reads make for covers.
type VersionMedia interface {
	// Covers resolves versions' covers.
	Covers(ctx context.Context, covers []versions.CoverOf) ([][]media.PublicRendition, error)
	// DeleteTx claims irreversible erasure under the eligibility row locks.
	DeleteTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error
	// Delete verifies physical deletion after that claim has committed.
	Delete(ctx context.Context, targets []versions.Target) error
	// ResumeDeletes completes late-write cleanup owned by content purge.
	ResumeDeletes(ctx context.Context, limit int) (int, error)
	// CheckRestorableTx refuses restoration once irreversible erasure started.
	CheckRestorableTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error
}

var errMediaNotConfigured = errors.New("catalog: media is not bound")

func (s *Store) checkRestorableTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if s.media == nil {
		return nil
	}
	err := s.media.Versions.CheckRestorableTx(ctx, tx, targets)
	if errors.Is(err, mediastore.ErrNotFound) {
		return errors.Join(ErrConflict, errors.New("version media is being permanently erased"))
	}
	return err
}

type chapterStartValidator interface {
	ValidateChapterStartsTx(context.Context, pgx.Tx, versions.Target, []string) error
}

type chapterLocker interface {
	WithChapterLocks(context.Context, []versions.Target, func() error) error
}

func (s *Store) withChapterLocks(ctx context.Context, targets []versions.Target, write func() error) error {
	if len(targets) == 0 || s.media == nil {
		return write()
	}
	locker, ok := s.media.Versions.(chapterLocker)
	if !ok {
		return errors.New("catalog: chapter writes require recovered manifest locks")
	}
	return locker.WithChapterLocks(ctx, targets, write)
}

func (s *Store) validateChapterStarts(ctx context.Context, tx pgx.Tx, version string, segments []catalog.Segment) error {
	var target versions.Target
	var kind string
	target.Version = version
	if err := tx.QueryRow(ctx, `SELECT v.content_id, c.kind FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=$2`, s.tenant, version).Scan(&target.Content, &kind); err != nil {
		return err
	}
	if s.cfg.Kinds[kind].Presentation != "reader" {
		return invalid("page-anchored chapters require reader content")
	}
	var validator chapterStartValidator
	if s.media != nil {
		validator, _ = s.media.Versions.(chapterStartValidator)
	}
	if validator == nil {
		return invalid("reader media must be bound before authoring page-anchored chapters")
	}
	starts := make([]string, len(segments))
	for i, segment := range segments {
		starts[i] = segment.StartPage
	}
	if err := validator.ValidateChapterStartsTx(ctx, tx, target, starts); err != nil {
		return invalid("chapter starts: %v", err)
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

// BodyImage takes a body image an editor uploaded to a version (Upload.Body):
// it returns its reference and an editor preview. No other version is changed.
func (s *Store) BodyImage(ctx context.Context, actor access.Actor, contentID string, in catalog.BodyImageInput) (catalog.InlineImage, error) {
	if in.Image == nil || !media.ValidNamed(*in.Image) {
		return catalog.InlineImage{}, invalid("image must be an inline image name (i-{uuid})")
	}
	if s.media == nil {
		return catalog.InlineImage{}, errMediaNotConfigured
	}
	g, err := s.grantsFor(ctx, actor, []string{contentID})
	if err != nil {
		return catalog.InlineImage{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return catalog.InlineImage{}, err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, []string{contentID}, g)
	if err == nil && got[contentID].state == catalog.StateDeleted {
		err = ErrNotFound
	}
	if err != nil {
		return catalog.InlineImage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = $2 AND state <> 'deleted'`, s.tenant, contentID)
	if err != nil {
		return catalog.InlineImage{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return catalog.InlineImage{}, err
	}
	if !slices.Contains(ids, in.Version) {
		return catalog.InlineImage{}, invalid("version %q is not a version of %s", in.Version, contentID)
	}
	if err := tx.Commit(ctx); err != nil {
		return catalog.InlineImage{}, err
	}
	ref, err := s.mediaRef(got[contentID].kind, contentID, in.Version)
	if err != nil {
		return catalog.InlineImage{}, err
	}
	url, err := s.previewURL(ctx, actor, ref, *in.Image)
	if err != nil {
		return catalog.InlineImage{}, err
	}
	out := catalog.InlineImage{Ref: ImageRef(*in.Image)}
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
