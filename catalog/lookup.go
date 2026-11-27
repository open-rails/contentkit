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
	// LiveFrom is when it opens to readers: the later of its first completed
	// release and its scheduled publication, kept through deletion; absent
	// while it is unpublished or unreleased. It may be in the future.
	LiveFrom *time.Time `json:"live_from,omitempty"`
}

// MaxVersionStatuses bounds VersionStatuses.
const MaxVersionStatuses = 100
