package discovery

import (
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/media"
)

// Card is one content item as a grid shows it, with the version that fits
// the reader best: of the versions the query matches, the one the display
// language opens by default (internal/edition), whose cover it shows. Of a
// version the reader does not open it shows what its kind's locked view does
// (kinds.Locked).
type Card struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Code and Path are its page, /{lang}/{route}/{code}/{slug}, which opens
	// the card's version: with ?v= when that is not the page's default.
	Code string `json:"code"`
	Path string `json:"path"`
	// Title is in Language: the display language, else a fallback.
	Title    string                  `json:"title"`
	Language string                  `json:"language"`
	Cover    []media.PublicRendition `json:"cover"`
	Version  CardVersion             `json:"version"`
	// Entities are the version's effective entities with a page, by entity kind.
	Entities map[string][]Chip `json:"entities"`
	// Channel is the original authoring home; reposts do not replace it.
	Channel     *channels.Card       `json:"channel,omitempty"`
	Repost      *channels.Repost     `json:"repost,omitempty"`
	Counts      Counts               `json:"counts"`
	Access      CardAccess           `json:"access"`
	Viewer      *catalog.ViewerState `json:"viewer,omitempty"`
	PublishedAt *time.Time           `json:"published_at,omitempty"`
	Attributes  map[string]any       `json:"attributes"`
	// UpdatedAt is when the work last gained something after this card's
	// version was published: a newer version, or pages added to this one
	// (new chapters); absent until then.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Deleted marks a deleted item or version for staff and its creators.
	Deleted bool `json:"deleted,omitempty"`
	// Excerpt is a post kind's plain-text summary, about 300 characters: its
	// description, else its body's opening for readers who open it.
	Excerpt string `json:"excerpt,omitempty"`
}

// CardVersion is the version a card shows; Code selects it on the card's
// page (?v=).
type CardVersion struct {
	ID         string   `json:"id"`
	Code       string   `json:"code"`
	Languages  []string `json:"languages"`
	Traits     []string `json:"traits"`
	Label      string   `json:"label,omitempty"`
	Pages      *int     `json:"pages,omitempty"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
	HasAudio   bool     `json:"has_audio"`
	// HLS is the folder of its ladder (a read's hls entry) once its video is
	// in, for a reader who opens it: the card previews it inline.
	HLS string `json:"hls,omitempty"`
}

// Chip names one entity on a card. Position is the content's sort key in an
// ordered entity: auto-filled from 1, but a host may set any value of 0 or more.
type Chip struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	Position *int   `json:"position,omitempty"`
}

// Counts are explicit reactions and favorites; reading quality is not a public open counter.
type Counts struct {
	Likes     *int `json:"likes,omitempty"`
	Dislikes  *int `json:"dislikes,omitempty"`
	Favorites *int `json:"favorites,omitempty"`
}

// CardAccess is the reader's access decision on the card's version.
type CardAccess struct {
	Allowed bool `json:"allowed"`
	// Level and Tiers are the version's effective rule, the same for every
	// reader (public once free): what a premium badge shows.
	Level access.Level `json:"level"`
	Tiers []string     `json:"tiers,omitempty"`
	// Unknown: entitlements could not be read; denied, nothing offered.
	Unknown bool `json:"unknown,omitempty"`
	// Sell are the keys whose purchase opens it; Requires is the key a buyer
	// holds first.
	Sell     []string         `json:"sell,omitempty"`
	Requires string           `json:"requires,omitempty"`
	Preview  *access.Preview  `json:"preview,omitempty"`
	Releases []access.Release `json:"releases,omitempty"`
}

// ContentPage is one page of cards.
type ContentPage struct {
	Popularity *PopularitySnapshot `json:"popularity,omitempty"`
	Contents   []Card              `json:"contents"`
	Summary    *ListingSummary     `json:"summary,omitempty"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}
