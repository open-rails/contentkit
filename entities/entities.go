// Package entities is how content is grouped: tags, artists, series,
// characters, channels, playlists and featured rails. Entity kinds come from
// kinds.Config.EntityKinds. A entity has localized names and aliases,
// per-language text, declared attributes, a cover, a parent and an optional
// scope; content joins it through memberships, at
// content level or for one version where the kind allows. A version's
// effective entities (EffectiveEntities) feed content_facets.entity_ids; counts
// derive from those facets.
package entities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/open-rails/contentkit/contentref"
)

var (
	ErrInvalid = errors.New("entities: invalid input")
)

// EntityID identifies one entity within a tenant: a UUIDv7, kept across import.
type EntityID = contentref.EntityID

// State of an entity. A merged entity points at its survivor (MergedInto);
// deleted entities keep their memberships but are effective nowhere.
type State string

const (
	StateActive State = "active"
)

const (
	StateMerged State = "merged"
)

const (
	StateDeleted State = "deleted"
)

// NameKind distinguishes the one canonical name per language from aliases.
type NameKind string

const (
	NameCanonical NameKind = "name"
)

const (
	NameAlias NameKind = "alias"
)

// Name is one localized canonical name or alias.
type Name struct {
	Language string   `json:"language"`
	Kind     NameKind `json:"kind"`
	Name     string   `json:"name"`
}

// Text is an entity's description and sort key in one language.
type Text struct {
	Language    string `json:"language"`
	Description string `json:"description,omitempty"`
	SortKey     string `json:"sort_key,omitempty"`
}

// Cover is an entity's picture: an image uploaded to the entity's media folder
// ("i-{uuid}") or a member content's cover, the cover of the edition a
// reader's language opens by default. Auto covers are the most-favorited live
// member, selected at read time when none is explicitly chosen.
type Cover struct {
	Image     string `json:"image,omitempty"`
	ContentID string `json:"content_id,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	// Renditions are its public images, filled on reads (Options.Covers).
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Avatar is an entity's square picture (kinds.EntityKind.HasAvatar): an image uploaded to the
// entity's media folder ("avatar/i-{uuid}"), selected by name. Without a
// selection a read shows the folder's first avatar, else the kind's default.
type Avatar struct {
	Image string `json:"image,omitempty"`
	// Renditions are its public images, filled on reads (Options.Covers).
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Rendition is one public image of a cover, as media.PublicRendition.
type Rendition struct {
	URL string `json:"url"`
	W   int    `json:"w"`
	H   int    `json:"h"`
}

// CoverResolver turns covers and avatars into their public renditions, per
// picture in order, one call per page: media/versions.Service.EntityCovers.
type CoverResolver interface {
	Covers(ctx context.Context, covers []CoverOf) ([][]Rendition, error)
}

// CoverOf is one entity's picture to resolve: its cover, or with Preset
// "avatar" its avatar (Cover.Image the selected one, if any). A content's
// cover is its default edition's in Language.
type CoverOf struct {
	Kind     string
	ID       EntityID
	Cover    Cover
	Language string
	Preset   string
}

// Entity is one entity record.
type Entity struct {
	ID   EntityID `json:"id"`
	Kind string   `json:"kind"`
	Slug string   `json:"slug"`
	// Internal marks a classification used by policy rather than public metadata.
	Internal bool `json:"internal"`
	// ScopeID is the entity the slug is unique within (a character's series).
	ScopeID     EntityID       `json:"scope_id,omitempty"`
	ParentID    EntityID       `json:"parent_id,omitempty"`
	State       State          `json:"state"`
	MergedInto  EntityID       `json:"merged_into,omitempty"`
	Attributes  map[string]any `json:"attributes"`
	Cover       *Cover         `json:"cover,omitempty"`
	Avatar      *Avatar        `json:"avatar,omitempty"`
	OwnerUserID string         `json:"-"`
	// AuthScope is the AuthKit group whose holders of
	// access.EntitySettingsManage edit it; ContentKit provisions it for a
	// kind with auth_scope (Options.Scopes).
	AuthScope string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Revision  int64     `json:"revision"`
}

// DefaultDeletedRetention is how long a deleted user-owned entity keeps its
// media folder, so a restore brings it back.
const DefaultDeletedRetention = 30 * 24 * time.Hour

// OwnedContent follows user-owned entities' deletion and restore with the
// content they own (owner_entity): the catalog. A nil entities is every entity.
type OwnedContent interface {
	// DeleteOwned deletes the live content of the deleted ones among entities,
	// recording each entity as its content's cause; bought content stays
	// readable to its buyers.
	DeleteOwned(ctx context.Context, ents []string) error
	// RestoreOwned restores the content the active ones among entities
	// deleted when they were, unless it was purged since; content deleted
	// for another reason stays deleted.
	RestoreOwned(ctx context.Context, ents []string) error
}

func normalizeLanguage(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" || len(l) > 16 || strings.IndexFunc(l, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("%w: language %q", ErrInvalid, l)
	}
	return l, nil
}
