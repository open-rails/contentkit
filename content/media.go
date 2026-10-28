package content

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

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
