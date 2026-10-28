package catalog

import (
	"time"
)

// VersionStatus is an edition's catalog state, for hosts that keep references
// to editions. A purged edition has none.
type VersionStatus struct {
	VersionID string   `json:"version_id"`
	ContentID string   `json:"content_id"`
	Kind      string   `json:"kind"`
	Languages []string `json:"languages"`
	// Deleted is the edition's or its content's deletion.
	Deleted bool `json:"deleted"`
	// Live is released and not deleted at the time of the read.
	Live bool `json:"live"`
	// LiveFrom is when its release completed, kept through deletion; absent
	// before release.
	LiveFrom *time.Time `json:"live_from,omitempty"`
}

// MaxVersionStatuses bounds VersionStatuses.
const MaxVersionStatuses = 100
