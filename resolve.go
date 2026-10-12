package contentkit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
)

// pollFolder is the media kind of poll images (content.Media's default).
const pollFolder = "poll"

// pollMedia is versions.Others: poll images, the one media kind neither the
// catalog nor groups own, through the content module.
type pollMedia struct{ e *Engine }

func (p pollMedia) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	var polls []contentref.ContentRef
	for _, ref := range refs {
		if ref.ContentKind == pollFolder && p.e.media.polls {
			polls = append(polls, ref)
		}
	}
	if len(polls) == 0 {
		return map[contentref.ContentKey]access.Resolution{}, nil
	}
	return p.e.content.MediaResolver().Resolve(ctx, polls, actor)
}

func (p pollMedia) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind != pollFolder || !p.e.media.polls {
		return media.UploadGrant{}, nil
	}
	return p.e.content.CanUpload(ctx, actor, t)
}

// groupCovers resolves group covers through media, which is built after
// groups.
type groupCovers struct{ e *Engine }

func (g groupCovers) Covers(ctx context.Context, covers []groups.CoverOf) ([][]groups.Rendition, error) {
	return g.e.media.versions.GroupCovers().Covers(ctx, covers)
}

// groupFolders deletes group media folders through media, built after
// groups.
type groupFolders struct{ e *Engine }

func (g groupFolders) DeleteTx(ctx context.Context, tx pgx.Tx, refs []groups.Ref) error {
	return g.e.media.versions.GroupFolders().DeleteTx(ctx, tx, refs)
}
