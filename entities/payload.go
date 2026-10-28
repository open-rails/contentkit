package entities

import (
	"github.com/open-rails/contentkit/access"
)

// AvatarPreset is the media preset avatars render in (media.AvatarPreset).
const AvatarPreset = "avatar"

// EntityAccess is an entity entitlement and the current viewer's standing.
// Product and price information belongs to the billing provider.
type EntityAccess struct {
	Key     access.Key `json:"key"`
	Member  bool       `json:"member"`
	Editor  bool       `json:"editor,omitempty"`
	Unknown bool       `json:"unknown,omitempty"`
}
