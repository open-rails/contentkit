package versions

import (
	"context"
	"slices"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

// DownloadPolicy is the host's download policy: whether a viewer may download
// a version now, and which pages of a page download it marks with its own
// watermark. ContentKit asks; the host decides.
type DownloadPolicy interface {
	// Decide rules on a download after the access check, before the quota.
	Decide(ctx context.Context, r DownloadRequest) (DownloadDecision, error)
	// Mark returns page's (from 1) marked bytes for actor and their
	// extension (".png"): a page Decide named.
	Mark(ctx context.Context, actor access.Actor, v Target, page int, b []byte) ([]byte, string, error)
}

// DownloadRequest is a download Decide rules on.
type DownloadRequest struct {
	Actor  access.Actor
	Tiers  []string // the tiers Actor holds
	Editor bool     // Actor edits the version
	Target
	Kind string
	// Language is the version's language; Title is its title, in Language
	// when it has one there (its content's id when it has none).
	Language, Title string
	// Entities are the version's entities, its own and its content's.
	Entities []DownloadEntity
	// Rule is the version's effective access rule.
	Rule access.Rule
	// Released is when the version went live; zero while it is not.
	Released time.Time
	// Pages are a page download's pages; 0 for a file.
	Pages int
}

// DownloadEntity is an entity of a download's version, named in its language,
// else the site's default, else another, else by its slug, with its
// attributes.
type DownloadEntity struct {
	kinds.EntityRef
	Name       string
	Attributes map[string]any
}

// In reports whether the version is in entity g.
func (r DownloadRequest) In(g kinds.EntityRef) bool {
	return slices.ContainsFunc(r.Entities, func(d DownloadEntity) bool { return d.EntityRef == g })
}

// DownloadDecision is a policy's ruling.
type DownloadDecision struct {
	Allowed bool
	// A refusal's stable code of the host's ("release_window") and, when it
	// is a matter of time, when the viewer may download.
	Reason      string
	AvailableAt *time.Time
	// Mark are the pages (from 1) of a page download to mark; files ignore it.
	Mark []int
	// Name is the download's file name without its extension; "" keeps
	// ContentKit's. ContentKit cleans and bounds it.
	Name string
}

// UnavailableError is a download the policy refused.
type UnavailableError struct {
	Reason      string
	AvailableAt *time.Time
}

func (e *UnavailableError) Error() string { return "versions: download unavailable (" + e.Reason + ")" }

// Downloads are the download quotas: an anonymous viewer's (by address), a
// signed-in one's, and the first of Tiers a viewer holds, which replaces
// User. A bucket without quotas is unlimited.
type Downloads struct {
	Anonymous []Quota     `json:"anonymous,omitempty" yaml:"anonymous"`
	User      []Quota     `json:"user,omitempty" yaml:"user"`
	Tiers     []TierQuota `json:"tiers,omitempty" yaml:"tiers"`
	// TTL is how long a file download's URL opens its file; 0 is DefaultDownloadTTL.
	TTL time.Duration `json:"ttl,omitempty" yaml:"ttl"`
}

// DefaultDownloadTTL is how long a file download's URL opens its file: long
// enough to start the transfer, which then runs to its end.
const DefaultDownloadTTL = 5 * time.Minute

// Quota admits at most Limit downloads in any Per.
type Quota struct {
	Limit int           `json:"limit" yaml:"limit"`
	Per   time.Duration `json:"per" yaml:"per"`
}

// TierQuota is the bucket of a tier's holders.
type TierQuota struct {
	Tier   string  `json:"tier" yaml:"tier"`
	Quotas []Quota `json:"quotas" yaml:"quotas"`
}

// QuotaError is a download refused by its viewer's quota.
type QuotaError struct {
	Bucket     string
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string { return "versions: download quota reached (" + e.Bucket + ")" }

// FileDownload is a granted download. URL serves it as an attachment named
// Name until ExpiresAt (a transfer begun runs to its end): the media host's
// URL of a file, or this module's archive route relative to the download
// route ("archive?..."). Size is 0 for archives: they stream on demand.
type FileDownload struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Size      int64     `json:"size,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ArchiveRequest is the signed download grant's selectors. Editor delegates
// only this exact transfer until ticket expiry, like a private media URL. It
// never creates a session or reusable permission checker. Claims retain only
// configured token-tier entitlements for this verified transfer.
type ArchiveRequest struct {
	Target
	Viewer, Account              string
	AccountSet                   bool
	Kind                         auth.SubjectKind
	Editor                       bool
	Claims                       []string
	Mark, Name, Revision, Ticket string
	Pages                        int  // exact authorized ZIP prefix; bound into the ticket
	File                         bool // the configured exact single-file download, never a caller-supplied path
}
