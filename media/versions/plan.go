package versions

import (
	"time"

	"github.com/open-rails/contentkit/media"
)

// ReaderPlan is a ready version snapshot with live chapter metadata. Released
// spans select existing files in the same canonical version; their URLs are
// signed on demand. A locked version's previews are its leading pages' public
// renditions.
type ReaderPlan struct {
	NextReleaseAt *time.Time    `json:"next_release_at,omitempty"`
	Content       string        `json:"content_id"`
	Version       string        `json:"version_id"`
	Publication   string        `json:"publication_id"`
	Pages         *int          `json:"pages,omitempty"`
	Segments      []PlanSegment `json:"segments"`
	Previews      []PlanPreview `json:"previews"`
	Playback      *PlanPlayback `json:"playback,omitempty"`
	Reader        *ReaderMeta   `json:"reader,omitempty"`
	Expires       int64         `json:"expires,omitempty"` // an inspection grant's expiry, Unix seconds
}

// PlanSegment is one span of pages: a segment's (Position set) or pages
// outside every segment.
type PlanSegment struct {
	Position   *int           `json:"position,omitempty"`
	Title      string         `json:"title,omitempty"`
	Start      int            `json:"start"`
	End        int            `json:"end"`
	Released   bool           `json:"released"`
	ReleaseAt  *time.Time     `json:"release_at,omitempty"`
	Locked     bool           `json:"locked"`
	Inspection bool           `json:"inspection,omitempty"` // authorized prepared derivatives; fetch with explicit management read
	Item       *media.RefBody `json:"item,omitempty"`       // read it at GET /media/{kind}/{id}
	Pages      []PlanPage     `json:"pages,omitempty"`
}

// PlanPage is one page of an open segment: its name, reading size and the
// path of each rendition in the segment's item.
type PlanPage struct {
	ID    string            `json:"id"` // original image SHA-256; the canonical page identity
	Name  string            `json:"name"`
	Span  *PageSpan         `json:"span,omitempty"`
	W     int               `json:"w"`
	H     int               `json:"h"`
	Files map[string]string `json:"files"`
}

// PlanPreview is a public preview of a leading page.
type PlanPreview struct {
	ID    string `json:"id"` // original image SHA-256
	Index int    `json:"index"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// PlanPlayback is the version's audio item and its tracks.
type PlanPlayback struct {
	Item       media.RefBody `json:"item"`
	Tracks     []PlanTrack   `json:"tracks"`
	Inspection bool          `json:"inspection,omitempty"`
}

type PlanTrack struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Duration float64 `json:"duration,omitempty"`
}
