package mediastore

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// StageCopies copies each request's From to a fresh staged upload of ref's
// item, so no byte passes through this process; a commit's puts adopt them
// as they do presigned uploads; tmp/ expires what none adopts.
func (u *Uploads) StageCopies(ctx context.Context, actor access.Actor, ref contentref.ContentRef, reqs []media.CopyRequest) ([]media.StagedCopy, error) {
	item, err := u.item(ref)
	if err != nil {
		return nil, err
	}
	if err := RequireWritable(ctx, u.o.Store); err != nil {
		return nil, err
	}
	out := make([]media.StagedCopy, 0, len(reqs))
	for _, r := range reqs {
		if r.Path == "" || r.Type == "" || r.From == "" || r.Size < 0 {
			return nil, uploadErr(media.CodeInvalid, "a copy needs a path, a type and a source")
		}
		if err := u.copySource(r.From); err != nil {
			return nil, err
		}
		path, g, err := u.uploadPath(item.Kind(), r.Path, r.Type)
		if err != nil {
			return nil, err
		}
		stem, _ := splitExt(path)
		grant, err := u.authorize(ctx, actor, media.UploadTarget{Ref: ref, Path: stem})
		if err != nil {
			return nil, err
		}
		s, err := u.stageCopy(ctx, actor, item, path, g, grant, r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// copySource refuses a source in ContentKit's media layout: an item's upload
// copies with OpCopy.
func (u *Uploads) copySource(from string) error {
	if _, ok := layout.Parse(from); ok {
		return uploadErr(media.CodeInvalid, "copy source %q is a ContentKit media key: use OpCopy", from)
	}
	return nil
}

// stageCopy copies r.From to a fresh staged upload at path (Upload g).
func (u *Uploads) stageCopy(ctx context.Context, actor access.Actor, item Item, path string, g int, grant media.UploadGrant, r media.CopyRequest) (media.StagedCopy, error) {
	src, err := u.o.Store.Head(ctx, r.From)
	if errors.Is(err, ErrNotFound) {
		return media.StagedCopy{}, uploadErr(media.CodeNotFound, "copy source %q not found", r.From)
	} else if err != nil {
		return media.StagedCopy{}, err
	}
	if r.Size > 0 && src.Size != r.Size {
		return media.StagedCopy{}, uploadErr(media.CodeInvalid, "copy source holds %d bytes; declared %d", src.Size, r.Size)
	}
	if err := allows(item.Kind().Uploads[g], r.Type, src.Size); err != nil {
		return media.StagedCopy{}, err
	}
	blob := NewStaged()
	key, _ := item.Staged(blob)
	obj, err := u.o.Store.Copy(ctx, r.From, key, media.CopyOptions{IfMatch: src.ETag, ContentType: r.Type})
	if err != nil {
		return media.StagedCopy{}, fmt.Errorf("media: copy %s: %w", r.From, err)
	}
	return media.StagedCopy{Path: path, Blob: blob, Size: obj.Size}, nil
}
