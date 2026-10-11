package content

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Media connects post and poll images to ContentKit media. The browser uploads
// an image to a Named upload path of the post's or poll's item (the host's
// media registry declares it and its public preset), and hands its name
// ("i-{uuid}") to ContentKit: covers and poll images store the name, a post
// body references it (ImageRef). Reads resolve names to the images' current
// public files, one MediaImages lookup per response. Post visibility changes
// reconcile public files; poll deletion removes the item. Runtime.CanUpload
// authorizes those uploads.
type Media struct {
	Images  MediaImages  // *media.Manifests
	Folders MediaFolders // *media.Jobs
	// Reader gives editors an unpublished post's image previews (POST
	// /posts/{id}/images); contentkit.NewRuntime fills it from its Reader.
	Reader   MediaReader
	PostKind string // media kind of post folders; default "post"
	PollKind string // media kind of poll folders; default "poll"
}

// MediaImages answers image queries from the current publications in one
// read; *media.Manifests implements it.
type MediaImages interface {
	Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error)
}

// MediaReader reads an item as the media read API does; *media.Reader
// implements it.
type MediaReader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
}

// MediaFolders queues visibility changes and folder deletions in the content
// transaction; *media.Jobs implements it.
type MediaFolders interface {
	ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error
	DeleteItemsTx(ctx context.Context, tx pgx.Tx, items ...media.Deletion) error
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
	if out.PostKind == "" {
		out.PostKind = "post"
	}
	if out.PollKind == "" {
		out.PollKind = "poll"
	}
	if out.PostKind == out.PollKind {
		return nil, fmt.Errorf("content: Media.PostKind and PollKind must differ")
	}
	return &out, nil
}

// folder selects a post's or a poll's media folder.
type folder int

const (
	postFolder folder = iota
	pollFolder
)

func (m *Media) kind(f folder) string {
	if f == postFolder {
		return m.PostKind
	}
	return m.PollKind
}

func (rt *Runtime) exposePostMediaTx(ctx context.Context, tx pgx.Tx, id string) error {
	if rt.media == nil {
		return nil
	}
	return rt.media.Folders.ExposeTx(ctx, tx, rt.Ref(rt.media.PostKind, id))
}

// deleteMediaTx queues a media folder's deletion with the row.
func (rt *Runtime) deleteMediaTx(ctx context.Context, tx pgx.Tx, f folder, id string) error {
	if rt.media == nil {
		return nil
	}
	return rt.media.Folders.DeleteItemsTx(ctx, tx, media.Deletion{Ref: rt.Ref(rt.media.kind(f), id)})
}

// CanUpload is media's UploadAuthorizer for post and poll items: the actor
// holds Perms.PostWrite or Perms.PollWrite and the post or poll exists in this
// tenant. Every other target is refused; hosts route their own kinds elsewhere.
func (rt *Runtime) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	ref := t.Ref
	if rt.media == nil || ref.TenantID != rt.tenant || ref.ContentVersionID != nil {
		return media.UploadGrant{}, nil
	}
	var perm, table string
	switch ref.ContentKind {
	case rt.media.PostKind:
		perm, table = rt.perms.PostWrite, rt.store.t.posts
	case rt.media.PollKind:
		if !uuidRe.MatchString(ref.ContentID) {
			return media.UploadGrant{}, nil
		}
		perm, table = rt.perms.PollWrite, rt.store.t.pollQuestions
	default:
		return media.UploadGrant{}, nil
	}
	if rt.requirePerm(ctx, actor, perm) != nil {
		return media.UploadGrant{}, nil
	}
	var id string
	err := rt.store.pool.QueryRow(ctx, `SELECT id::text FROM `+table+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`,
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

// MediaResolver is the access.ContentResolver of the post and poll media
// kinds: route the host registry's Hooks.Resolver to it for Media.PostKind and
// PollKind, as CanUpload is routed. A post's folder shows like the post: a
// published one to everyone, a draft, scheduled, held or rejected one only to
// its author and PostWrite holders, its editors (an editor read gives them
// the images before anyone else sees them), a deleted one to no one. A poll's
// folder shows while the poll exists, edited by PollWrite holders. Other refs
// are omitted (denied).
func (rt *Runtime) MediaResolver() access.ContentResolver { return mediaResolver{rt} }

type mediaResolver struct{ rt *Runtime }

func (m mediaResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	rt := m.rt
	out := make(map[contentref.ContentKey]access.Resolution, len(refs))
	if rt.media == nil {
		return out, nil
	}
	var posts, polls []string
	for _, ref := range refs {
		if ref.TenantID != rt.tenant || ref.ContentVersionID != nil {
			continue
		}
		switch ref.ContentKind {
		case rt.media.PostKind:
			posts = append(posts, ref.ContentID)
		case rt.media.PollKind:
			if uuidRe.MatchString(ref.ContentID) {
				polls = append(polls, ref.ContentID)
			}
		}
	}
	signedIn := !actor.Anonymous && actor.ID != ""
	can := func(perm string) bool { return signedIn && rt.requirePerm(ctx, actor, perm) == nil }
	if len(posts) > 0 {
		postWrite := can(rt.perms.PostWrite)
		rows, err := rt.store.pool.Query(ctx, `SELECT id, author_id, NOT is_draft AND moderation = 'approved' AND (live_at IS NULL OR live_at <= now())
			FROM `+rt.store.t.posts+` WHERE tenant_id = $1 AND id = ANY($2) AND deleted_at IS NULL`, rt.tenant, posts)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, author string
			var published bool
			if err := rows.Scan(&id, &author, &published); err != nil {
				return nil, err
			}
			editor := postWrite || (signedIn && actor.ID == author)
			if published || editor {
				out[rt.Ref(rt.media.PostKind, id).Key()] = access.Resolution{Visible: true, Accessible: true, Editor: editor}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(polls) > 0 {
		pollWrite := can(rt.perms.PollWrite)
		rows, err := rt.store.pool.Query(ctx, `SELECT id::text FROM `+rt.store.t.pollQuestions+`
			WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND deleted_at IS NULL`, rt.tenant, polls)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[rt.Ref(rt.media.PollKind, id).Key()] = access.Resolution{Visible: true, Accessible: true, Editor: pollWrite}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
