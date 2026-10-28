package discovery

import (
	"github.com/open-rails/contentkit/entities"
)

// ContentFilterState is one configured content filter as the reader has it.
type ContentFilterState struct {
	Entity         entities.EntityCard `json:"entity"`
	DefaultVisible bool                `json:"default_visible"`
	// Visible is the reader's setting, Overridden whether it is their own.
	Visible    bool `json:"visible"`
	Overridden bool `json:"overridden"`
}

// ContentFilters offers one control per entity across the selected content kinds.
type ContentFilters struct {
	AppliesTo      []string             `json:"applies_to"`
	Filters        []ContentFilterState `json:"filters"`
	IncludeDeleted bool                 `json:"include_deleted"`
}

// ContentFiltersUpdate atomically changes only submitted choices. Omitted
// entities and include_deleted retain their values, including across tabs.
type ContentFiltersUpdate struct {
	Filters        []GuestFilterSetting `json:"filters,omitempty"`
	IncludeDeleted *bool                `json:"include_deleted,omitempty"`
}

type ContentFiltersInitialization struct {
	AccountID      string               `json:"account_id"`
	Filters        []GuestFilterSetting `json:"filters"`
	IncludeDeleted bool                 `json:"include_deleted"`
}

// GuestFilterSetting is a shared entity choice or reset without pinning a domain's
// current default value. Exactly one of Visible or Default:true is required.
type GuestFilterSetting struct {
	EntityID string `json:"entity_id"`
	Visible  *bool  `json:"visible,omitempty"`
	Default  *bool  `json:"default,omitempty"`
}
