package catalog

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/offers"
)

// Errors; match with errors.Is.
var (
	ErrInvalid   = errors.New("catalog: invalid request")
	ErrNotFound  = errors.New("catalog: not found")
	ErrForbidden = errors.New("catalog: forbidden")
	ErrConflict  = errors.New("catalog: conflict")
)

// Content states and listings.
const (
	StateDraft     = "draft"
	StatePublished = "published"
	StateDeleted   = "deleted"

	ListingListed   = "listed"
	ListingMembers  = "members"
	ListingUnlisted = "unlisted"
)

// Text is a content item's text in one language. Body is rich text whose
// inline images are "contentkit:i-{uuid}" references (ImageRef).
type Text struct {
	Title       string   `json:"title"`
	Slug        string   `json:"slug,omitempty"`
	Description string   `json:"description,omitempty"`
	Body        string   `json:"body,omitempty"`
	SortKey     string   `json:"sort_key,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// VersionText overrides a version's title and description in one language.
type VersionText struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Creator is a person credited on a content item; creators edit it.
type Creator struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role,omitempty"`
	Position int    `json:"position"`
}

// Segment is an ordered, individually released range of a version (a
// chapter): items [Start, End). A segment is released once it and every
// earlier one are past ReleaseAt.
type Segment struct {
	Position  int        `json:"position"`
	Title     string     `json:"title,omitempty"`
	Start     int        `json:"start"`
	End       int        `json:"end"`
	ReleaseAt *time.Time `json:"release_at,omitempty"`
	// Released is computed on reads.
	Released bool `json:"released"`
}

// Cover is a content item's cover: an inline image of its media folder.
type Cover struct {
	Image string  `json:"image"`
	URL   *string `json:"url,omitempty"`
}

// Decision is a viewer's access verdict on a version.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Unknown: entitlements could not be read; denied, nothing offered.
	Unknown bool `json:"unknown,omitempty"`
	// Sell is the keys a paywall offers; Requires the key a buyer holds first.
	Sell     []string `json:"sell,omitempty"`
	Requires string   `json:"requires,omitempty"`
	// Preview is what a locked viewer still reads; FreeAfter when it opens to all.
	Preview   *access.Preview `json:"preview,omitempty"`
	FreeAfter *time.Time      `json:"free_after,omitempty"`
	// Offers are the purchasable offers of Sell's keys; tiers are the host's
	// to label.
	Offers []offers.Sale `json:"offers,omitempty"`
}

// Version is what a viewer opens.
type Version struct {
	ID        string   `json:"id"`
	Position  int      `json:"position"`
	Languages []string `json:"languages"`
	Traits    []string `json:"traits"`
	Label     string   `json:"label,omitempty"`
	// Title and Description are the override in the detail's language.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// State is the editors' to see.
	State       string     `json:"state,omitempty"`
	PublishAt   *time.Time `json:"publish_at,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	// Live: published, past PublishAt, its content live.
	Live bool `json:"live"`
	// Access is the version's own rule; nil inherits the content's.
	Access     *access.Rule   `json:"access,omitempty"`
	Attributes map[string]any `json:"attributes"`
	Pages      *int           `json:"pages,omitempty"`
	DurationMS *int64         `json:"duration_ms,omitempty"`
	Width      *int           `json:"width,omitempty"`
	Height     *int           `json:"height,omitempty"`
	HasAudio   bool           `json:"has_audio"`
	Ordinal    *int           `json:"ordinal,omitempty"`
	Segments   []Segment      `json:"segments"`
	Decision   Decision       `json:"decision"`
	// Text is every language's override, for editors.
	Text map[string]VersionText `json:"text,omitempty"`
	// Offer is the version's current offer, for its offer editors.
	Offer *offers.Offer `json:"offer,omitempty"`
}

// GroupSummary names one group a content item belongs to.
type GroupSummary struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Content is a content item as a viewer sees it: its text in the viewer's
// language (falling back), its live versions (every version for editors),
// groups and attributes.
type Content struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Code and Path are its page: [/{lang}]/{route}/{code}/{slug}.
	Code string `json:"code"`
	Path string `json:"path"`
	// Language is the language of the text below.
	Language    string `json:"language"`
	Title       string `json:"title"`
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description,omitempty"`
	// Body shows each inline image with a current public file as its URL;
	// only editors and viewers Decision allows get it. Description is the
	// teaser everyone reads.
	Body string `json:"body,omitempty"`
	// Decision is the viewer's access to the item: allowed when they open any
	// of its versions, else its first version's paywall.
	Decision Decision `json:"decision"`
	// Images maps each resolved image name of Body and the cover to its URL.
	Images       map[string]string `json:"images,omitempty"`
	Cover        *Cover            `json:"cover,omitempty"`
	State        string            `json:"state"`
	Listing      string            `json:"listing"`
	PublishAt    *time.Time        `json:"publish_at,omitempty"`
	PublishedAt  *time.Time        `json:"published_at,omitempty"`
	Live         bool              `json:"live"`
	Access       access.Rule       `json:"access"`
	Restrictions []string          `json:"restrictions"`
	Attributes   map[string]any    `json:"attributes"`
	OwnerGroupID string            `json:"owner_group_id,omitempty"`
	// Owner is the owner group's card (a channel's name, path, avatar and
	// cover) while it is active.
	Owner    *groups.GroupCard `json:"owner,omitempty"`
	Creators []Creator         `json:"creators"`
	Groups   []GroupSummary    `json:"groups"`
	Versions []Version         `json:"versions"`
	// Editor: the viewer may edit it; the fields below are theirs.
	Editor           bool            `json:"editor"`
	Text             map[string]Text `json:"text,omitempty"`
	Moderation       string          `json:"moderation,omitempty"`
	ModerationReason string          `json:"moderation_reason,omitempty"`
	EverSold         bool            `json:"ever_sold,omitempty"`
	// Offer is the item's current offer, for its offer editors.
	Offer     *offers.Offer `json:"offer,omitempty"`
	Revision  int64         `json:"revision"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	DeletedAt *time.Time    `json:"deleted_at,omitempty"`
}

// ImageScheme is the URL scheme of an inline image reference in a body.
const ImageScheme = "contentkit:"

var imageRefRe = regexp.MustCompile(ImageScheme + `(i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// ImageRef is the body reference to an inline image name ("i-{uuid}").
func ImageRef(name string) string { return ImageScheme + name }

func imageNames(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range imageRefRe.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func resolveBody(body string, urls map[string]string) string {
	if len(urls) == 0 {
		return body
	}
	return imageRefRe.ReplaceAllStringFunc(body, func(ref string) string {
		if u, ok := urls[ref[len(ImageScheme):]]; ok {
			return u
		}
		return ref
	})
}

type stripTags struct{}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (stripTags) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(tagRe.ReplaceAllString(raw, "")), nil
}
