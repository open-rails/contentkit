package media

import (
	"github.com/open-rails/contentkit/contentref"
)

// ImageQuery selects an item's current public images: all of them, or only
// those of a Preset, of an upload Name (its path's {name}, e.g. an inline
// image's "i-{uuid}"), or both.
type ImageQuery struct {
	Ref    contentref.ContentRef
	Preset string
	Name   string
}
