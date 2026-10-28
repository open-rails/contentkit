package discovery

import (
	"context"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// Media is the cards' media: versions.Service. Covers resolves covers to
// public renditions, per cover in order; Ladder is the folder of a kind's
// HLS ladder (a read's hls entry), "" when it streams none.
type Media interface {
	Covers(ctx context.Context, covers []versions.CoverOf) ([][]media.PublicRendition, error)
	Ladder(kind string) string
}
