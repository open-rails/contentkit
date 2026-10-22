// Package importer is ContentKit's bulk import, POST /v1/import: how a legacy
// importer writes content, groups, interactions and signals. Records are
// idempotent by their (source, entity, external id) in content_external_ids
// and keep the ids they give, so re-running an import converges: changed rows
// are updated and nothing is duplicated. A record that fails is reported and
// the rest are written; sending it again resumes. Counts, group counts and
// search documents are rebuilt by the engine's jobs, which an import queues.
// Media is not imported here: importers ingest files, covers and body images
// included, into each version's folders through the media upload API
// (docs/import.md).
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
	PreferenceFloor int64 `json:"preference_floor,omitempty"`
	// ProvisionScopes lets ContentKit create the AuthKit scope of an
	// imported group of an auth_scope kind that names none; without it such
	// a group fails.
	ProvisionScopes bool                  `json:"provision_scopes,omitempty"`
	Groups          []GroupRecord         `json:"entities,omitempty"`
	Contents        []ContentRecord       `json:"contents,omitempty"`
	Memberships     []MembershipSet       `json:"memberships,omitempty"`
	Offers          []OfferRecord         `json:"offers,omitempty"`
	Aliases         []AliasRecord         `json:"aliases,omitempty"`
	Comments        []CommentRecord       `json:"comments,omitempty"`
	Reactions       []ReactionRecord      `json:"reactions,omitempty"`
	Favorites       []FavoriteRecord      `json:"favorites,omitempty"`
	Polls           []PollRecord          `json:"polls,omitempty"`
	Votes           []VoteRecord          `json:"votes,omitempty"`
	Answers         []AnswerRecord        `json:"answers,omitempty"`
	LegacyReading   []LegacyReadingRecord `json:"legacy_reading,omitempty"`
	Signals         []SignalRecord        `json:"signals,omitempty"`
	// Deletes, written last, remove records gone from the source. Records
	// are never deleted by their absence from a batch.
	Deletes []DeleteRecord `json:"deletes,omitempty"`
}

// Ref names a record by its ContentKit id or by its external id under the
// batch's source; the entity is the field's.
type Ref struct {
	ID         string `json:"id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

// TargetRef is what an interaction, offer or alias is about: a content item,
// one version (its content is implied), a group or a comment, as the
// section allows.
type TargetRef struct {
	Content *Ref `json:"content,omitempty"`
	Version *Ref `json:"version,omitempty"`
	Group   *Ref `json:"group,omitempty"`
	Comment *Ref `json:"comment,omitempty"`
}

// GroupRecord is a group with its names and text. Nil lists keep what is
// stored; given ones replace it.
type GroupRecord struct {
	ExternalID string `json:"external_id"`
	// ID is kept when the external id is new (contentref.LegacyID derives a
	// stable one); generated when empty.
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	// Internal classifications can drive domain policy but never public metadata.
	// Omission preserves an existing entity's classification; new entities are public.
	Internal *bool `json:"internal,omitempty"`
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
	// Avatar is a user-owned group's avatar: an image of its media folder.
	Avatar string `json:"avatar,omitempty"`
	// OwnerUserID owns a user-owned kind's group.
	OwnerUserID string `json:"owner_user_id,omitempty"`
	// AuthScope is the group's AuthKit scope (an auth_scope kind's); empty
	// keeps the stored one.
	AuthScope string `json:"auth_scope,omitempty"`
	// Edges replace the group's edges to other groups.
	Edges []EdgeRecord `json:"edges,omitempty"`
	// Code keeps a content code the group had (9 characters); one is
	// generated otherwise. A code never changes once given.
	Code      string     `json:"code,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// EdgeRecord is a relation from the group to another.
type EdgeRecord struct {
	Relation string `json:"relation"`
	To       Ref    `json:"to"`
}

// ContentRecord is a content item with its text, creators and versions. Nil
// text, creators, version text and segments keep what is stored; given ones
// replace it. Versions not listed are left as they are.
type ContentRecord struct {
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
	// Nil preserves an existing source title/language; a pointer to empty clears it.
	OriginalTitle    *string `json:"original_title,omitempty"`
	OriginalLanguage *string `json:"original_language,omitempty"`
	// Comments is open (the default), closed or hidden.
	Comments string `json:"comments,omitempty"`
	// Access defaults to public. A group rule names the group's id.
	Access       *access.Rule            `json:"access,omitempty"`
	Restrictions []string                `json:"restrictions,omitempty"`
	Attributes   map[string]any          `json:"attributes,omitempty"`
	OwnerGroup   *Ref                    `json:"owner_group,omitempty"`
	Creators     []catalog.Creator       `json:"creators,omitempty"`
	Text         map[string]catalog.Text `json:"text,omitempty"`
	// Versions: a kind with hidden versions takes none or one.
	Versions  []VersionRecord `json:"versions,omitempty"`
	Code      string          `json:"code,omitempty"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
}

// VersionRecord is one version of a content item, ordered as listed.
type VersionRecord struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	// Code is the version code (?v=) to keep, as Code is the content's: empty
	// keeps the stored one or assigns one.
	Code      string   `json:"code,omitempty"`
	Languages []string `json:"languages"`
	Traits    []string `json:"traits,omitempty"`
	Label     string   `json:"label,omitempty"`
	// State is published (the default), draft or deleted; times as ContentRecord's.
	State       string         `json:"state,omitempty"`
	PublishAt   *time.Time     `json:"publish_at,omitempty"`
	PublishedAt *time.Time     `json:"published_at,omitempty"`
	DeletedAt   *time.Time     `json:"deleted_at,omitempty"`
	Access      *access.Rule   `json:"access,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
	Ordinal     *int           `json:"ordinal,omitempty"`
	// Cover selects one of the version's cover uploads (i-{uuid}): a
	// record's one legacy cover is uploaded to each of its versions. Left
	// out, the stored selection stays.
	Cover     string                         `json:"cover,omitempty"`
	Text      map[string]catalog.VersionText `json:"text,omitempty"`
	Segments  []catalog.Segment              `json:"segments,omitempty"`
	CreatedAt *time.Time                     `json:"created_at,omitempty"`
	UpdatedAt *time.Time                     `json:"updated_at,omitempty"`
}

// MembershipSet replaces a content item's memberships in the listed group
// kinds; memberships in other kinds stay.
type MembershipSet struct {
	Content     Ref                `json:"content"`
	Kinds       []string           `json:"kinds"`
	Memberships []MembershipRecord `json:"memberships"`
}

// MembershipRecord places the content item, or one version of it, in a
// group.
type MembershipRecord struct {
	Group       Ref        `json:"group"`
	Version     *Ref       `json:"version,omitempty"`
	Role        string     `json:"role,omitempty"`
	Position    *int       `json:"position,omitempty"`
	ActiveFrom  *time.Time `json:"active_from,omitempty"`
	ActiveUntil *time.Time `json:"active_until,omitempty"`
	// State is active (the default) or proposed.
	State string `json:"state,omitempty"`
}

// OfferRecord puts a content item, a version or a sellable group on sale; the
// offers job syncs it to the billing provider.
type OfferRecord struct {
	Target TargetRef    `json:"target"`
	Terms  offers.Terms `json:"terms"`
}

// AliasRecord resolves another system's identifier (under the batch's source) to
// a content item or group: a legacy URL's id, token or name.
type AliasRecord struct {
	// Kind is the identifier space ("folder", "tag-name").
	Kind string `json:"kind"`
	Key  string `json:"key"`
	// Locator is a position inside the target, such as a page.
	Locator string    `json:"locator,omitempty"`
	Target  TargetRef `json:"target"`
}

// CommentRecord is a comment on a content item (or one version), or a reply
// to a top-level comment on it; a reply to a reply joins its thread.
type CommentRecord struct {
	ExternalID string    `json:"external_id"`
	ID         string    `json:"id,omitempty"`
	Target     TargetRef `json:"target"`
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

// ReactionRecord is a person's like (1), dislike (-1) or neutral (0) on a
// content item, version, comment or group; an anonymous one is keyed by IP.
type ReactionRecord struct {
	Target    TargetRef  `json:"target"`
	UserID    string     `json:"user_id,omitempty"`
	IP        string     `json:"ip,omitempty"`
	Value     int16      `json:"value"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// FavoriteRecord is a person's bookmark of a content item or version.
type FavoriteRecord struct {
	Target    TargetRef  `json:"target"`
	UserID    string     `json:"user_id"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// PollRecord is a site poll with its options, which replace the stored ones.
type PollRecord struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	// Kind is multiple_choice (the default) or free_text.
	Kind     string `json:"kind,omitempty"`
	Question string `json:"question"`
	Language string `json:"language,omitempty"`
	// Active defaults to true.
	Active    *bool              `json:"active,omitempty"`
	LiveAt    *time.Time         `json:"live_at,omitempty"`
	ClosesAt  *time.Time         `json:"closes_at,omitempty"`
	DeletedAt *time.Time         `json:"deleted_at,omitempty"`
	CreatedAt *time.Time         `json:"created_at,omitempty"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
	Options   []PollOptionRecord `json:"options,omitempty"`
}

// PollOptionRecord is one choice of a multiple-choice poll, ordered by Position.
type PollOptionRecord struct {
	ExternalID string `json:"external_id"`
	ID         string `json:"id,omitempty"`
	Label      string `json:"label"`
	Position   int    `json:"position"`
}

// VoteRecord is one person's (or, anonymous, one IP's) choice in a poll.
type VoteRecord struct {
	Poll      Ref        `json:"poll"`
	Option    Ref        `json:"option"`
	UserID    string     `json:"user_id,omitempty"`
	IP        string     `json:"ip,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// AnswerRecord is one person's free-text answer to a poll; the classifier
// groups it later.
type AnswerRecord struct {
	Poll      Ref        `json:"poll"`
	UserID    string     `json:"user_id"`
	Text      string     `json:"text"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// LegacyReadingRecord preserves source bookmarks/progress as legacy state,
// never as measured attention or popularity evidence.
type LegacyReadingRecord struct {
	Target      TargetRef  `json:"target"`
	UserID      string     `json:"user_id,omitempty"`
	AnonKey     string     `json:"anon_key,omitempty"`
	FirstReadAt *time.Time `json:"first_read_at,omitempty"`
	LastReadAt  time.Time  `json:"last_read_at"`
	Progress    uint32     `json:"progress"`
	ProgressMax uint32     `json:"progress_max"`
	Completed   bool       `json:"completed"`
	Resume      string     `json:"resume,omitempty"`
	Revision    uint64     `json:"revision,omitempty"`
}

// SignalRecord imports an explicit click. Existing bookmarks/progress use
// LegacyReadingRecord; raw opens and imported counters are not accepted.
type SignalRecord struct {
	Target TargetRef `json:"target"`
	// UserID or, signed out, an opaque AnonKey.
	UserID  string `json:"user_id,omitempty"`
	AnonKey string `json:"anon_key,omitempty"`
	// Type is click; imported raw opens are rejected.
	Type string `json:"type"`
	// EventID is the event's stable identity: re-sending it adds nothing.
	EventID    string    `json:"event_id"`
	Revision   uint64    `json:"revision,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// DeleteRecord deletes a record the source no longer has, as its delete
// route would: a content item, version, group, comment or poll, by its
// external id under the batch's source. One already deleted (or since
// erased) stays as it is, so re-sending converges; an external id never
// imported fails not_found.
type DeleteRecord struct {
	// Entity is content, version, group, comment or poll.
	Entity     string `json:"entity"`
	ExternalID string `json:"external_id"`
}

// Sections, in the order a batch writes them.
const (
	SectionGroups        = "entities"
	SectionContents      = "contents"
	SectionMemberships   = "memberships"
	SectionOffers        = "offers"
	SectionAliases       = "aliases"
	SectionComments      = "comments"
	SectionReactions     = "reactions"
	SectionFavorites     = "favorites"
	SectionPolls         = "polls"
	SectionVotes         = "votes"
	SectionAnswers       = "answers"
	SectionLegacyReading = "legacy_reading"
	SectionSignals       = "signals"
	SectionDeletes       = "deletes"
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
	Skipped map[string]int  `json:"skipped,omitempty"`
	Failed  []RecordFailure `json:"failed"`
	// IDs maps each written record's external id to its id, by entity.
	IDs map[string]map[string]string `json:"ids"`
}

// RecordFailure is one record not written. Sending it again (or the whole
// batch) once its cause is fixed resumes the import.
type RecordFailure struct {
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
