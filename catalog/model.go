package catalog

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/media"
)

// Errors; match with errors.Is.
var (
	ErrInvalid   = errors.New("catalog: invalid request")
	ErrNotFound  = errors.New("catalog: not found")
	ErrForbidden = errors.New("catalog: forbidden")
	ErrConflict  = errors.New("catalog: conflict")
)

// Content states.
const (
	StateDraft     = "draft"
	StatePublished = "published"
	StateDeleted   = "deleted"
)

// VersionText is one edition's authored text in one language. Inline images
// resolve only against this edition's media; no sibling or parent fallback.
type VersionText struct {
	Title       string   `json:"title"`
	Slug        string   `json:"slug,omitempty"`
	Description string   `json:"description,omitempty"`
	Body        string   `json:"body,omitempty"`
	SortKey     string   `json:"sort_key,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// Creator is a person credited on a content item. Credits do not grant access
// or editing authority; those follow the authenticated author and channel roles.
type Creator struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role,omitempty"`
	Position int    `json:"position"`
}

// Segment is a chapter of one version. StartPage names its first file in
// manifest order; the next chapter's marker or the end of the manifest ends it.
// ReleaseAt is intent. ReleasedAt is set only after durable publication.
type Segment struct {
	ID         string     `json:"id"`
	Position   int        `json:"position"`
	Title      string     `json:"title,omitempty"`
	StartPage  string     `json:"start_page,omitempty"`
	Start      int        `json:"start"`
	End        int        `json:"end"`
	ReleaseAt  *time.Time `json:"release_at,omitempty"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	Released   bool       `json:"released"`
}

// SegmentInput authors a chapter by its first virtual filename in the manifest.
// ID preserves the chapter across title, ordering and marker edits. Numeric
// range authoring is rejected; Start and End exist only on displayed chapters
// and the separate trusted import model.
type SegmentInput struct {
	ID        string     `json:"id,omitempty"`
	Position  int        `json:"position,omitempty"`
	Title     string     `json:"title,omitempty"`
	StartPage string     `json:"start_page,omitempty"`
	Start     int        `json:"-"` // rejected legacy Go input; never a wire field
	End       int        `json:"-"` // rejected legacy Go input; never a wire field
	ReleaseAt *time.Time `json:"release_at,omitempty"`
	Released  bool       `json:"released,omitempty"` // ignored, retained for older clients
}

// Cover is a version's cover: Image is the cover upload its editors
// selected ("i-{uuid}"; empty shows its first, else its kind's default);
// URL is its widest public rendition.
type Cover struct {
	Image      string                  `json:"image,omitempty"`
	URL        *string                 `json:"url,omitempty"`
	Renditions []media.PublicRendition `json:"renditions,omitempty"`
}

// Decision is a viewer's access verdict on a version.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Unknown: entitlements could not be read; denied, nothing offered.
	Unknown bool `json:"unknown,omitempty"`
	// Sell is the keys a paywall offers; Requires the key a buyer holds first.
	Sell     []string `json:"sell,omitempty"`
	Requires string   `json:"requires,omitempty"`
	// Preview is what a locked viewer still reads; Releases describe additional audiences.
	Preview  *access.Preview  `json:"preview,omitempty"`
	Releases []access.Release `json:"releases,omitempty"`
}

// Version is what a viewer opens.
type Version struct {
	ID string `json:"id"`
	// Code selects it on its content's page (?v=): unique within the content,
	// fixed for the version's life.
	Code      string   `json:"code"`
	Position  int      `json:"position"`
	Languages []string `json:"languages"`
	Traits    []string `json:"traits"`
	Label     string   `json:"label,omitempty"`
	// Resolved text is selected from this version only in the request language.
	Title            string            `json:"title,omitempty"`
	Description      string            `json:"description,omitempty"`
	Language         string            `json:"language,omitempty"`
	Slug             string            `json:"slug,omitempty"`
	Body             string            `json:"body,omitempty"`
	Images           map[string]string `json:"images,omitempty"`
	OriginalTitle    string            `json:"original_title,omitempty"`
	OriginalLanguage string            `json:"original_language,omitempty"`
	// State is the editors' to see.
	State       string     `json:"state,omitempty"`
	PublishAt   *time.Time `json:"publish_at,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	// Publication is editor-only durable work status. A pending update does
	// not replace the edition's already released reader state.
	Publication *PublicationStatus `json:"publication,omitempty"`
	// Live reports a completed release under the current publication state.
	Live bool `json:"live"`
	// Access is the version's own rule. Noneditors do not receive this field.
	Access     *access.Rule   `json:"access,omitempty"`
	Attributes map[string]any `json:"attributes"`
	// Entities are entities attached specifically to this version. Combine them
	// with Content.Entities when presenting the selected version's credits.
	Entities   []EntitySummary `json:"entities"`
	Pages      *int            `json:"pages,omitempty"`
	DurationMS *int64          `json:"duration_ms,omitempty"`
	Width      *int            `json:"width,omitempty"`
	Height     *int            `json:"height,omitempty"`
	HasAudio   bool            `json:"has_audio"`
	Ordinal    *int            `json:"ordinal,omitempty"`
	// Cover is absent when its kind's locked view hides it from the viewer.
	Cover    *Cover    `json:"cover,omitempty"`
	Segments []Segment `json:"segments"`
	Decision Decision  `json:"decision"`
	// Text is every authored language of this edition, for editors.
	Text map[string]VersionText `json:"text,omitempty"`
	// Viewer is the signed-in reader's standing with the version.
	Viewer *ViewerState `json:"viewer,omitempty"`
}

// PublicationStatus is the latest accepted edition publication operation.
// Its state describes work; Version.Live describes current reader availability.
type PublicationStatus struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// ViewerState is a signed-in reader's own standing with a content item or
// version: their favorite and, with a signal plane, where they are in it.
type ViewerState struct {
	Favorited bool `json:"favorited"`
	// Reaction is their like (1) or dislike (-1) of the item; absent for neither.
	Reaction int `json:"reaction,omitempty"`
	// Seen, Completed, Progress, ProgressMax and Resume (the client's own
	// pointer, such as a page) come from the signal plane. Progress counts
	// whole pages for a reader and seconds for a player.
	Seen        bool       `json:"seen"`
	Completed   bool       `json:"completed"`
	Progress    uint32     `json:"progress"`
	ProgressMax uint32     `json:"progress_max"`
	Resume      string     `json:"resume,omitempty"`
	LastReadAt  *time.Time `json:"last_read_at,omitempty"`
}

// EntitySummary names one entity a content item belongs to; Code and Path are
// its page, as content's are, when its kind has one.
type EntitySummary struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
	Path string `json:"path,omitempty"`
}

// Content is a resolved reader response. Text and media fields are read-only
// projections of SelectedVersionID; the parent stores no text. It includes live
// versions (every version for editors),
// entities and attributes. A viewer who opens none of its versions gets what
// its kind's locked view shows (kinds.Locked), and of each locked version
// what that view shows.
type Content struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Code and Path are its page: /{lang}/{route}/{code}/{slug}.
	Code string `json:"code"`
	Path string `json:"path"`
	// Language is the localized text language. Title may instead come from the
	// selected edition or OriginalTitle under the kind's title policy.
	Language string `json:"language"`
	Title    string `json:"title"`
	// OriginalTitle and OriginalLanguage project the selected version only.
	OriginalTitle    string `json:"original_title,omitempty"`
	OriginalLanguage string `json:"original_language,omitempty"`
	// Slug is Path's decorative tail in the page's language (entity and
	// title, "/"-joined); Slugs is it per language. Links built from them
	// equal Path.
	Slug        string            `json:"slug,omitempty"`
	Slugs       map[string]string `json:"slugs,omitempty"`
	Description string            `json:"description,omitempty"`
	// Body shows each inline image with its selected edition's signed URL;
	// only editors and viewers Decision allows get it. Description is the
	// teaser everyone reads.
	Body string `json:"body,omitempty"`
	// Decision is the viewer's access to the item: allowed when they open any
	// of its versions, else its first version's paywall.
	Decision Decision `json:"decision"`
	// Images maps each resolved image name of Body to its URL: the body
	// images of the edition the viewer opens (DefaultVersion among them).
	Images map[string]string `json:"images,omitempty"`
	// Cover is the cover of the edition the request's language opens by
	// default (DefaultVersion): what cards and meta tags show.
	Cover *Cover `json:"cover,omitempty"`
	State string `json:"state"`
	// Comments is open, closed or hidden.
	Comments     string         `json:"comments"`
	PublishAt    *time.Time     `json:"publish_at,omitempty"`
	PublishedAt  *time.Time     `json:"published_at,omitempty"`
	Live         bool           `json:"live"`
	Access       access.Rule    `json:"access"`
	Restrictions []string       `json:"restrictions"`
	Attributes   map[string]any `json:"attributes"`
	ChannelID    string         `json:"channel_id,omitempty"`
	// Channel identifies the original's primary authoring channel while active.
	Channel  *channels.Card `json:"channel,omitempty"`
	Creators []Creator      `json:"creators"`
	// Entities are shared/null-version memberships. Version-specific entities
	// are carried by each Version.Entities, never merged across editions here.
	Entities []EntitySummary `json:"entities"`
	Versions []Version       `json:"versions"`
	// SelectedVersionID is the edition selected for this response: the direct
	// version lookup's edition, or the default among the visible versions.
	SelectedVersionID string `json:"selected_version_id,omitempty"`
	// Viewer is the signed-in reader's standing with the item, to resume
	// it; each version carries its own.
	Viewer *ViewerState `json:"viewer,omitempty"`
	// Editor: the viewer may edit it; the fields below are theirs.
	Editor    bool       `json:"editor"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// DefaultVersion is the version a reader of language opens by default, the
// one rule cards and contentkit-ui's defaultVersion follow (internal/edition):
// the first live version serving language, else the first live one, else
// the first; vs in position order. Nil when vs is empty.
func DefaultVersion(vs []Version, language string) *Version {
	rank := func(v *Version) int {
		r := 0
		if v.Live {
			r += 2
		}
		if language != "" && slices.Contains(v.Languages, language) {
			r++
		}
		return r
	}
	var best *Version
	for i := range vs {
		if best == nil || rank(&vs[i]) > rank(best) {
			best = &vs[i]
		}
	}
	return best
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
