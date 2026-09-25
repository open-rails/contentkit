package content

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Media connects post and poll images to ContentKit media. The browser uploads
// an image through media's upload API as an inline image of the post's or
// poll's folder ({tenant}/{kind}/{id}/, whose media.Kind sets Inline), waits
// for it to render and hands its name ("i-{uuid}") to ContentKit, which
// stores the public URL and deletes the folder with the post or poll. Runtime.CanUpload authorizes those
// uploads.
type Media struct {
	URLs     MediaURLs    // *media.Reader
	Folders  MediaFolders // *media.Jobs
	PostKind string       // media kind of post folders; default "post"
	PollKind string       // media kind of poll folders; default "poll"
}

// MediaURLs resolves inline images to their public URLs (media.ErrPending
// until rendered); *media.Reader implements it.
type MediaURLs interface {
	InlineURL(ctx context.Context, ref contentref.ContentRef, name string) (string, error)
}

// MediaFolders deletes item folders from the host's transaction; *media.Jobs
// implements it.
type MediaFolders interface {
	DeleteItemsTx(ctx context.Context, tx pgx.Tx, items ...media.Deletion) error
}

var errMediaNotConfigured = errors.New("content: Options.Media is not configured")

func newMedia(m *Media) (*Media, error) {
	if m == nil {
		return nil, nil
	}
	if m.URLs == nil || m.Folders == nil {
		return nil, fmt.Errorf("content: Media needs URLs and Folders")
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

// imageURL resolves an inline image name of a post or poll folder to its
// public URL; "" clears the image (nil).
func (rt *Runtime) imageURL(ctx context.Context, f folder, id, name string) (*string, error) {
	if rt.media == nil {
		return nil, errMediaNotConfigured
	}
	if name == "" {
		return nil, nil
	}
	if !layout.ValidInlineName(name) {
		return nil, badRequest("image must be an inline image name (i-{uuid})")
	}
	u, err := rt.media.URLs.InlineURL(ctx, rt.Ref(rt.media.kind(f), id), name)
	if errors.Is(err, media.ErrPending) {
		return nil, badRequest("image %s is not rendered yet", name)
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// deleteMediaTx deletes a post's or poll's media folder with the row.
func (rt *Runtime) deleteMediaTx(ctx context.Context, tx pgx.Tx, f folder, id string) error {
	if rt.media == nil {
		return nil
	}
	return rt.media.Folders.DeleteItemsTx(ctx, tx, media.Deletion{Ref: rt.Ref(rt.media.kind(f), id)})
}

// CanUpload is media's UploadAuthorizer for post and poll folders: the actor
// holds Perms.PostWrite or Perms.PollWrite and the post or poll exists in this
// tenant. Every other ref is refused; hosts route their own kinds elsewhere.
func (rt *Runtime) CanUpload(ctx context.Context, actor access.Actor, ref contentref.ContentRef) (media.UploadGrant, error) {
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

var _ media.UploadAuthorizer = (*Runtime)(nil)
