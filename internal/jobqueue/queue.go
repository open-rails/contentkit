// Package jobqueue defines app-owned queues and shared purge time bounds.
package jobqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const (
	ContentPurgeTimeout = 15 * time.Minute
	// Leave time for the last media batch and the catalog/entity completion scans.
	MediaCleanupBudget = ContentPurgeTimeout * 2 / 3
)

// For binds a queue or periodic identity to the immutable catalog partition.
// A bounded hex suffix keeps names within River's queue-name grammar even
// when a legacy catalog key uses characters unsuitable for queue names.
func For(base, catalog string) string {
	sum := sha256.Sum256([]byte(catalog))
	return base + "_" + hex.EncodeToString(sum[:16])
}
