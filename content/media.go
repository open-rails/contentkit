package content

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// MediaImages answers image queries from the item cache in one read;
// *mediastore.PGCache implements it.
type MediaImages interface {
	Images(ctx context.Context, qs ...media.ImageQuery) ([][]media.PublicImage, error)
}

// Avatars are people's avatars, from the item cache in one read;
// *mediastore.PGCache implements it.
type Avatars interface {
	Avatars(ctx context.Context, ids ...string) ([]media.PublicImage, error)
}

// MediaReader reads an item as the media read API does; *media.Reader
// implements it.
type MediaReader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
}
