package catalog

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Reader reads an item as the media read API does.
type Reader interface {
	Read(ctx context.Context, ref contentref.ContentRef, actor access.Actor, o media.ReadOptions) (*media.ReadResult, error)
	// NamedURLs signs an opened item's named images that have no public files.
	NamedURLs(ctx context.Context, ref contentref.ContentRef, actor access.Actor, names []string) (map[string]string, error)
}

// BodyImageInput names a body image uploaded to one version (Upload.Body).
type BodyImageInput struct {
	Image   *string `json:"image"`
	Version string  `json:"version"`
}
