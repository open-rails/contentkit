package contentkit

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Post and poll images live in these media kinds (content.Media's defaults).
const (
	postFolder = "post"
	pollFolder = "poll"
)

// owns reports a reference ContentKit answers for itself: post and poll
// images, when the registry declares their kinds.
func (e *Engine) owns(ref contentref.ContentRef) bool {
	return e.media != nil && e.media.folders && (ref.ContentKind == postFolder || ref.ContentKind == pollFolder)
}

// engineResolver is the one access.ContentResolver content and media consume:
// ContentKit's own posts and polls, then the host's (Deps.Resolver). The
// catalog's resolver (visibility plus Gate.Decide, #120) replaces the host's
// here, and Deps.Resolver and Deps.CanUpload go.
type engineResolver struct {
	e    *Engine
	host access.ContentResolver
}

func (r engineResolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	var own, host []contentref.ContentRef
	for _, ref := range refs {
		if r.e.owns(ref) {
			own = append(own, ref)
		} else {
			host = append(host, ref)
		}
	}
	out := make(map[contentref.ContentKey]access.Resolution, len(refs))
	if len(own) > 0 {
		res, err := r.e.content.MediaResolver().Resolve(ctx, own, actor)
		if err != nil {
			return nil, err
		}
		for k, v := range res {
			out[k] = v
		}
	}
	if len(host) > 0 && r.host != nil {
		res, err := r.host.Resolve(ctx, host, actor)
		if err != nil {
			return nil, err
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

// engineUploader authorizes uploads to ContentKit's own items itself and
// asks the host (Deps.CanUpload) for the rest.
type engineUploader struct {
	e    *Engine
	host media.UploadAuthorizer
}

func (u engineUploader) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if u.e.owns(t.Ref) {
		return u.e.content.CanUpload(ctx, actor, t)
	}
	if u.host == nil {
		return media.UploadGrant{}, nil
	}
	return u.host.CanUpload(ctx, actor, t)
}
