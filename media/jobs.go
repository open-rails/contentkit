package media

import (
	"github.com/open-rails/contentkit/contentref"
)

// Deletion is one item to delete. Owner is its quota owner (UploadGrant.Owner),
// "" for none; with a Limiter configured the owner's usage is released.
// OperationID is required for Purge; queued deletion uses its River job id.
type Deletion struct {
	Ref         contentref.ContentRef
	Owner       string
	OperationID string // required by Purge with a quota owner; stable across retries
}
