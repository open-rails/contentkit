// Package channels manages authoring homes and their public identity. Channels
// are not metadata: their AuthKit groups govern their original content items.
package channels

import (
	"time"

	"github.com/open-rails/contentkit/entities"
)

// Card is a channel's public identity and optional paid membership standing.
type Card struct {
	ID           string                 `json:"id"`
	Slug         string                 `json:"slug"`
	Name         string                 `json:"name"`
	NameLanguage string                 `json:"name_language,omitempty"`
	Code         string                 `json:"code,omitempty"`
	Path         string                 `json:"path,omitempty"`
	Cover        *entities.Cover        `json:"cover,omitempty"`
	Avatar       *entities.Avatar       `json:"avatar,omitempty"`
	Count        int                    `json:"count"`
	Access       *entities.EntityAccess `json:"access,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
}

// Channel includes editable identity and metadata. Metadata grants no authority.
type Channel struct {
	Card
	State       entities.State        `json:"state"`
	OwnerUserID string                `json:"owner_user_id,omitempty"`
	AuthScope   string                `json:"auth_scope,omitempty"`
	Revision    int64                 `json:"revision"`
	UpdatedAt   time.Time             `json:"updated_at"`
	Names       []entities.Name       `json:"names"`
	Text        []entities.Text       `json:"text"`
	Counts      []entities.Count      `json:"counts"`
	Entities    []entities.EntityCard `json:"entities"`
}

type Page struct {
	Channels   []Card `json:"channels"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      *int   `json:"total,omitempty"`
}

// Mine includes current authoring permissions, not the creator's past authority.
type Mine struct {
	Card
	Owner      bool `json:"owner"`
	Editor     bool `json:"editor"`
	CanPost    bool `json:"can_post"`
	CanEditOwn bool `json:"can_edit_own"`
	Writer     bool `json:"writer"`
	Moderator  bool `json:"moderator"`
	Member     bool `json:"member"`
}

type MyPage struct {
	Channels   []Mine `json:"channels"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type Input struct {
	Slug   string           `json:"slug"`
	Names  []entities.Name  `json:"names,omitempty"`
	Text   []entities.Text  `json:"text,omitempty"`
	Cover  *entities.Cover  `json:"cover,omitempty"`
	Avatar *entities.Avatar `json:"avatar,omitempty"`
}

type Update struct {
	ID          string           `json:"-"`
	OwnerUserID *string          `json:"owner_user_id,omitempty"`
	Slug        *string          `json:"slug,omitempty"`
	Names       []entities.Name  `json:"names,omitempty"`
	Text        []entities.Text  `json:"text,omitempty"`
	Cover       *entities.Cover  `json:"cover,omitempty"`
	Avatar      *entities.Avatar `json:"avatar,omitempty"`
	State       *entities.State  `json:"state,omitempty"`
}

// Repost identifies the destination reference without copying original content.
type Repost struct {
	ChannelID string    `json:"channel_id"`
	ActorID   string    `json:"actor_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CanRemove bool      `json:"can_remove"`
}
