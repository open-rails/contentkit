// Package importer is ContentKit's bulk import, POST /v1/import: how a legacy
// importer writes content, groups, interactions and signals. Records are
// idempotent by their (source, entity, external id) in content_external_ids
// and keep the ids they give, so re-running an import converges: changed rows
// are updated and nothing is duplicated. A record that fails is reported and
// the rest are written; sending it again resumes. Counts, group counts and
// search documents are rebuilt by the engine's jobs, which an import queues.
// Media is not imported here: importers ingest files into each version's
// folder through the media upload API (docs/import.md).
package importer

import (
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/offers"
)

// MaxRecords bounds the records of one batch, nested versions and options
// included.
const MaxRecords = 20_000

// Batch is one import call. Its sections are written in field order, each in
// transactions of up to a thousand records; a record may name what an
// earlier section or an earlier call wrote, by id or by external id.
type Batch struct {
	// Source names the importing system ("doujins-legacy"): external ids and
	// aliases are scoped by it.
	Source string `json:"source"`
	// PreferenceFloor raises the preference revision sequence past a
	// retained signal plane's newest revision before anything is stamped, as
	// after a Postgres restore. It only ever advances.
	PreferenceFloor int64         `json:"preference_floor,omitempty"`
	Groups          []Group       `json:"groups,omitempty"`
	Contents        []Content     `json:"contents,omitempty"`
	Memberships     []Memberships `json:"memberships,omitempty"`
	Offers          []Offer       `json:"offers,omitempty"`
	Aliases         []Alias       `json:"aliases,omitempty"`
	Comments        []Comment     `json:"comments,omitempty"`
	Reactions       []Reaction    `json:"reactions,omitempty"`
	Favorites       []Favorite    `json:"favorites,omitempty"`
	Polls           []Poll        `json:"polls,omitempty"`
	Votes           []Vote        `json:"votes,omitempty"`
	Answers         []Answer      `json:"answers,omitempty"`
	Signals         []Signal      `json:"signals,omitempty"`
}

// Ref names a record by its ContentKit id or by its external id under the
// batch's source; the entity is the field's.
type Ref struct {
	ID         string `json:"id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

// Target is what an interaction, offer or alias is about: a content item,
// one version (its content is implied), a group or a comment, as the
// section allows.
type Target struct {
	Content *Ref `json:"content,omitempty"`
	Version *Ref `json:"version,omitempty"`
	Group   *Ref `json:"group,omitempty"`
	Comment *Ref `json:"comment,omitempty"`
}

// Group is a group with its names and text. Nil lists keep what is stored;
// given ones replace it.
type Group struct {
	ExternalID string `json:"external_id"`
	// ID is kept when the external id is new (contentref.LegacyID derives a
	// stable one); generated when empty.
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	// Scope is the group a scoped kind's slug is unique within.
	Scope  *Ref `json:"scope,omitempty"`
	Parent *Ref `json:"parent,omitempty"`
	// State is active (the default) or deleted.
	State      string         `json:"state,omitempty"`
	Names      []groups.Name  `json:"names,omitempty"`
	Text       []groups.Text  `json:"text,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	// CoverImage is an image of the group's media folder (i-{uuid});
	// CoverContent a member whose cover it shows, imported before.
	CoverImage   string `json:"cover_image,omitempty"`
	CoverContent *Ref   `json:"cover_content,omitempty"`
	OwnerUserID  string `json:"owner_user_id,omitempty"`
	AuthScope    string `json:"auth_scope,omitempty"`
	// Edges replace the group's edges to other groups.
	Edges []Edge `json:"edges,omitempty"`
	// Code keeps a content code the group had (9 characters); one is
	// generated otherwise. A code never changes once given.
	Code      string     `json:"code,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Edge is a relation from the group to another.
type Edge struct {
	Relation string `json:"relation"`
	To       Ref    `json:"to"`
}

// Content is a content item with its text, creators and versions. Nil text,
// creators, version text and segments keep what is stored; given ones
// replace it. Versions not listed are left as they are.
type Content struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	Kind       string `json:"kind"`
	// State is draft, published or deleted. A published item is live from
	// PublishAt (now when omitted; scheduled when future); PublishedAt is
	// when it went live.
	State       string     `json:"state"`
	PublishAt   *time.Time `json:"publish_at,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	// DeletedAt dates a deleted item (now when omitted).
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Listing is listed (the default), members or unlisted.
	Listing string `json:"listing,omitempty"`
	// Access defaults to public. A group rule names the group's id.
	Access       *access.Rule   `json:"access,omitempty"`
	Restrictions []string       `json:"restrictions,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	OwnerGroup   *Ref           `json:"owner_group,omitempty"`
	// Cover is an inline image name of the item's media folder (i-{uuid}).
	Cover    string                  `json:"cover,omitempty"`
	Creators []catalog.Creator       `json:"creators,omitempty"`
	Text     map[string]catalog.Text `json:"text,omitempty"`
	// Versions: a kind with hidden versions takes none or one.
	Versions  []Version  `json:"versions,omitempty"`
	Code      string     `json:"code,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Version is one version of a content item, ordered as listed.
type Version struct {
	ExternalID string   `json:"external_id"`
	ID         string   `json:"id,omitempty"`
	Languages  []string `json:"languages"`
	Traits     []string `json:"traits,omitempty"`
	Label      string   `json:"label,omitempty"`
	// State is published (the default), draft or deleted; times as Content's.
	State       string                         `json:"state,omitempty"`
	PublishAt   *time.Time                     `json:"publish_at,omitempty"`
	PublishedAt *time.Time                     `json:"published_at,omitempty"`
	DeletedAt   *time.Time                     `json:"deleted_at,omitempty"`
	Access      *access.Rule                   `json:"access,omitempty"`
	Attributes  map[string]any                 `json:"attributes,omitempty"`
	Ordinal     *int                           `json:"ordinal,omitempty"`
	Text        map[string]catalog.VersionText `json:"text,omitempty"`
	Segments    []catalog.Segment              `json:"segments,omitempty"`
	CreatedAt   *time.Time                     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time                     `json:"updated_at,omitempty"`
}

// Memberships replaces a content item's memberships in the listed group
// kinds; memberships in other kinds stay.
type Memberships struct {
	Content     Ref          `json:"content"`
	Kinds       []string     `json:"kinds"`
	Memberships []Membership `json:"memberships"`
}

// Membership places the content item, or one version of it, in a group.
type Membership struct {
	Group       Ref        `json:"group"`
	Version     *Ref       `json:"version,omitempty"`
	Role        string     `json:"role,omitempty"`
	Position    *int       `json:"position,omitempty"`
	ActiveFrom  *time.Time `json:"active_from,omitempty"`
	ActiveUntil *time.Time `json:"active_until,omitempty"`
	// State is active (the default) or proposed.
	State string `json:"state,omitempty"`
}

// Offer puts a content item, a version or a sellable group on sale; the
// offers job syncs it to the billing provider.
type Offer struct {
	Target Target       `json:"target"`
	Terms  offers.Terms `json:"terms"`
}

// Alias resolves another system's identifier (under the batch's source) to
// a content item or group: a legacy URL's id, token or name.
type Alias struct {
	// Kind is the identifier space ("folder", "tag-name").
	Kind string `json:"kind"`
	Key  string `json:"key"`
	// Locator is a position inside the target, such as a page.
	Locator string `json:"locator,omitempty"`
	Target  Target `json:"target"`
}

// Comment is a comment on a content item (or one version), or a reply to a
// top-level comment on it; a reply to a reply joins its thread.
type Comment struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	Target     Target `json:"target"`
	// Language is the language the comment is written in; empty is unknown.
	Language string `json:"language,omitempty"`
	ReplyTo  *Ref   `json:"reply_to,omitempty"`
	// UserID or, for an anonymous comment, AnonName.
	UserID    string     `json:"user_id,omitempty"`
	AnonName  string     `json:"anon_name,omitempty"`
	Body      string     `json:"body"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Reaction is a person's like (1), dislike (-1) or neutral (0) on a content
// item, version, comment or group; an anonymous one is keyed by IP.
type Reaction struct {
	Target    Target     `json:"target"`
	UserID    string     `json:"user_id,omitempty"`
	IP        string     `json:"ip,omitempty"`
	Value     int16      `json:"value"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Favorite is a person's bookmark of a content item or version.
type Favorite struct {
	Target    Target     `json:"target"`
	UserID    string     `json:"user_id"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Poll is a site poll with its options, which replace the stored ones.
type Poll struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	// Kind is multiple_choice (the default) or free_text.
	Kind     string `json:"kind,omitempty"`
	Question string `json:"question"`
	Language string `json:"language,omitempty"`
	// Active defaults to true.
	Active    *bool        `json:"active,omitempty"`
	LiveAt    *time.Time   `json:"live_at,omitempty"`
	ClosesAt  *time.Time   `json:"closes_at,omitempty"`
	DeletedAt *time.Time   `json:"deleted_at,omitempty"`
	CreatedAt *time.Time   `json:"created_at,omitempty"`
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
	Options   []PollOption `json:"options,omitempty"`
}

// PollOption is one choice of a multiple-choice poll, ordered by Position.
type PollOption struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	Label      string `json:"label"`
	Position   int    `json:"position"`
}

// Vote is one person's (or, anonymous, one IP's) choice in a poll.
type Vote struct {
	Poll      Ref        `json:"poll"`
	Option    Ref        `json:"option"`
	UserID    string     `json:"user_id,omitempty"`
	IP        string     `json:"ip,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Answer is one person's free-text answer to a poll; the classifier groups
// it later.
type Answer struct {
	Poll      Ref        `json:"poll"`
	UserID    string     `json:"user_id"`
	Text      string     `json:"text"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Signal is a view or click on a content item or one version, for the
// signal plane; skipped without one. A view on a version counts for its
// content too.
type Signal struct {
	Target Target `json:"target"`
	// UserID or, signed out, an opaque AnonKey.
	UserID  string `json:"user_id,omitempty"`
	AnonKey string `json:"anon_key,omitempty"`
	// Type is view or click.
	Type string `json:"type"`
	// EventID is the event's stable identity: re-sending it adds nothing.
	EventID    string    `json:"event_id"`
	Revision   uint64    `json:"revision,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	DurationS  uint32    `json:"duration_s,omitempty"`
	// Progress is pages read (reader kinds), seconds watched (player kinds)
	// or percent scrolled (post kinds).
	Progress uint32 `json:"progress,omitempty"`
	Resume   string `json:"resume,omitempty"`
}

// Sections, in the order a batch writes them.
const (
	SectionGroups      = "groups"
	SectionContents    = "contents"
	SectionMemberships = "memberships"
	SectionOffers      = "offers"
	SectionAliases     = "aliases"
	SectionComments    = "comments"
	SectionReactions   = "reactions"
	SectionFavorites   = "favorites"
	SectionPolls       = "polls"
	SectionVotes       = "votes"
	SectionAnswers     = "answers"
	SectionSignals     = "signals"
)

// Entities of external ids.
const (
	EntityContent = "content"
	EntityVersion = "version"
	EntityGroup   = "group"
	EntityComment = "comment"
	EntityPoll    = "poll"
	EntityOption  = "poll_option"
)

// Result reports a batch: what each section wrote, the records that failed
// and the ids of the records with external ids.
type Result struct {
	Written map[string]int `json:"written"`
	// Skipped counts records an engine without the capability leaves out:
	// signals without a signal plane, offers without billing.
	Skipped map[string]int `json:"skipped,omitempty"`
	Failed  []Failure      `json:"failed"`
	// IDs maps each written record's external id to its id, by entity.
	IDs map[string]map[string]string `json:"ids"`
}

// Failure is one record not written. Sending it again (or the whole batch)
// once its cause is fixed resumes the import.
type Failure struct {
	Section    string `json:"section"`
	Index      int    `json:"index"`
	ExternalID string `json:"external_id,omitempty"`
	// Code is invalid, not_found, conflict, erased or internal.
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Failure codes.
const (
	CodeInvalid  = "invalid"
	CodeNotFound = "not_found"
	CodeConflict = "conflict"
	CodeErased   = "erased"
	CodeInternal = "internal"
)
