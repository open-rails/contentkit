package content

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

// Media connects poll images to ContentKit media. The
// browser uploads an image to a Named upload path of the poll's item (the
// host's media registry declares it and its public preset), and hands its
// name ("i-{uuid}") to ContentKit, which stores it. Reads resolve names to the
// images' current public files, one MediaImages lookup per response; poll
// deletion removes the item. Runtime.CanUpload authorizes those uploads.
type Media struct {
	Namespace string       // media partition; empty retains the content catalog key
	Images    MediaImages  // *media.Manifests
	Folders   MediaFolders // *media.Jobs
	// Reader reads items as the media read API does; the engine fills it
	// from its Reader.
	Reader   MediaReader
	PollKind string // media kind of poll folders; default "poll"
}

// MediaImages answers image queries from the current publications in one
// read; *media.Manifests implements it.
type MediaImages interface {
	Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error)
}

// Avatars are people's avatars, built from their ids without a lookup;
// *media.Registry implements it.
type Avatars interface {
	Avatars(ids ...string) []media.PublicImage
}

// MediaReader reads an item as the media read API does; *media.Reader
// implements it.
type MediaReader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
}

// MediaFolders records exact retirement intents in the poll mutation
// transaction; the existing content-deletion sweep performs storage erasure.
type MediaFolders interface {
	RetireItemsTx(ctx context.Context, tx pgx.Tx, items ...media.Deletion) error
}

var errMediaNotConfigured = errors.New("content: Options.Media is not configured")

func newMedia(m *Media) (*Media, error) {
	if m == nil {
		return nil, nil
	}
	if m.Images == nil || m.Folders == nil {
		return nil, fmt.Errorf("content: Media needs Images and Folders")
	}
	out := *m
	if out.PollKind == "" {
		out.PollKind = "poll"
	}
	return &out, nil
}

// folder selects a media folder: a poll's.
type folder int

const pollFolder folder = 0

func (m *Media) kind(folder) string { return m.PollKind }

// deleteMediaTx commits a retirement intent with the poll tombstone.
func (rt *Runtime) deleteMediaTx(ctx context.Context, tx pgx.Tx, f folder, id string) error {
	if rt.media == nil {
		return nil
	}
	return rt.media.Folders.RetireItemsTx(ctx, tx, media.Deletion{Ref: rt.mediaRef(rt.media.kind(f), id)})
}

// CanUpload is media's UploadAuthorizer for poll items: the actor holds
// root:polls:write and the poll exists in this tenant. Every other target is
// refused; other kinds are routed elsewhere.
func (rt *Runtime) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	ref := t.Ref
	if rt.media == nil || ref.TenantID != rt.mediaTenant() || ref.ContentVersionID != nil {
		return media.UploadGrant{}, nil
	}
	if ref.ContentKind != rt.media.PollKind || !uuidRe.MatchString(ref.ContentID) {
		return media.UploadGrant{}, nil
	}
	if rt.requirePerm(ctx, actor, access.PollsWrite) != nil {
		return media.UploadGrant{}, nil
	}
	var id string
	err := rt.store.pool.QueryRow(ctx, `SELECT id::text FROM `+rt.store.t.pollQuestions+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND kind='multiple_choice'`,
		ref.ContentID, rt.tenant).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return media.UploadGrant{}, nil
	}
	return media.UploadGrant{Allowed: err == nil && id == ref.ContentID}, err // folders use the canonical id
}

var (
	_ media.UploadAuthorizer = (*Runtime)(nil)
	_ MediaImages            = (*media.Manifests)(nil)
	_ MediaReader            = (*media.Reader)(nil)
)

// MediaResolver is the resolve.Resolver of the poll media kind: route
// the host registry's Hooks.Resolver to it for Media.PollKind, as CanUpload
// is routed. A poll's folder shows while the poll exists, edited by
// root:polls:write holders. Other refs are omitted (denied).
func (rt *Runtime) MediaResolver() resolve.Resolver { return mediaResolver{rt} }

type mediaResolver struct{ rt *Runtime }

func (m mediaResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	rt := m.rt
	out := make(map[contentref.ContentKey]resolve.Resolution, len(refs))
	if rt.media == nil {
		return out, nil
	}
	var polls []string
	for _, ref := range refs {
		if ref.TenantID == rt.mediaTenant() && ref.ContentVersionID == nil && ref.ContentKind == rt.media.PollKind && uuidRe.MatchString(ref.ContentID) {
			polls = append(polls, ref.ContentID)
		}
	}
	signedIn := !actor.Anonymous && actor.ID != ""
	can := func(perm access.Permission) bool { return signedIn && rt.requirePerm(ctx, actor, perm) == nil }
	if len(polls) > 0 {
		pollWrite := can(access.PollsWrite)
		rows, err := rt.store.pool.Query(ctx, `SELECT id::text FROM `+rt.store.t.pollQuestions+`
			WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND deleted_at IS NULL AND kind='multiple_choice'`, rt.tenant, polls)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[rt.mediaRef(rt.media.PollKind, id).Key()] = resolve.Resolution{Visible: true, Accessible: true, Editor: pollWrite}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (rt *Runtime) mediaTenant() string {
	if rt.media != nil && rt.media.Namespace != "" {
		return rt.media.Namespace
	}
	return rt.tenant
}
func (rt *Runtime) mediaRef(kind, id string) contentref.ContentRef {
	return contentref.New(rt.mediaTenant(), kind, id)
}
