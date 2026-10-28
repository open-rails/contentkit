package entities

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// EntityInput creates one entity.
type EntityInput struct {
	// ID is optional; omitted ids are generated.
	ID   EntityID `json:"id,omitempty"`
	Kind string   `json:"kind"`
	Slug string   `json:"slug"`
	// Internal classifications are visible only through management reads.
	Internal bool `json:"internal,omitempty"`
	// ScopeID is required for a kind scoped by another (a character's series).
	ScopeID    EntityID       `json:"scope_id,omitempty"`
	ParentID   EntityID       `json:"parent_id,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Cover      *Cover         `json:"cover,omitempty"`
	Avatar     *Avatar        `json:"avatar,omitempty"`
	// OwnerUserID owns a user-owned entity; a person creating one owns it.
	OwnerUserID string `json:"-"`
	// AuthScope is staff's (an import's); ContentKit provisions it otherwise.
	AuthScope string `json:"-"`
	Names     []Name `json:"names,omitempty"`
	Text      []Text `json:"text,omitempty"`
}

// EntityUpdate changes one entity's given fields; nil fields are unchanged.
type EntityUpdate struct {
	// ID names the entity; routes take it from the path.
	ID       EntityID  `json:"-"`
	Slug     *string   `json:"slug,omitempty"`
	Internal *bool     `json:"internal,omitempty"`
	ScopeID  *EntityID `json:"scope_id,omitempty"`
	// ParentID "" detaches the entity from its parent.
	ParentID *EntityID `json:"parent_id,omitempty"`
	// State restores (active) or deletes an entity.
	State *State `json:"state,omitempty"`
	// Attributes replaces every attribute.
	Attributes map[string]any `json:"attributes,omitempty"`
	// Cover selects a cover; an empty one returns to the automatic cover.
	Cover *Cover `json:"cover,omitempty"`
	// Avatar selects the entity's avatar (kinds with avatars); an empty one returns to
	// the first.
	Avatar      *Avatar `json:"avatar,omitempty"`
	OwnerUserID *string `json:"-"`
	AuthScope   *string `json:"-"`
	// Names replaces every name and alias.
	Names []Name `json:"names,omitempty"`
	// Text replaces the text of every language.
	Text []Text `json:"text,omitempty"`
}

type textRow struct {
	EntityID    string `json:"entity_id"`
	Language    string `json:"language"`
	Description string `json:"description"`
	SortKey     string `json:"sort_key"`
}

func validateText(id EntityID, text []Text) ([]textRow, error) {
	rows := make([]textRow, 0, len(text))
	seen := map[string]bool{}
	for _, t := range text {
		lang, err := normalizeLanguage(t.Language)
		if err != nil {
			return nil, err
		}
		if seen[lang] {
			return nil, fmt.Errorf("%w: two texts for %s", ErrInvalid, lang)
		}
		seen[lang] = true
		if utf8.RuneCountInString(t.Description) > 20000 || utf8.RuneCountInString(t.SortKey) > 512 {
			return nil, fmt.Errorf("%w: text for %s is too long", ErrInvalid, lang)
		}
		rows = append(rows, textRow{EntityID: string(id), Language: lang, Description: t.Description, SortKey: strings.TrimSpace(t.SortKey)})
	}
	return rows, nil
}
