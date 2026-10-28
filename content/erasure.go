package content

import (
	"context"
)

// ProviderDataEraser deletes personal data retained by optional moderation providers. Success means a durable tenant/subject fence is
// installed: Screen calls begun before erasure cannot recreate data
// when they complete later. Merely queuing a delete is not success. Calls are
// idempotent; implementations must preserve fences across restore/restart.
// A host using multiple retaining providers must implement a composite eraser.
type ProviderDataEraser interface {
	EraseSubjects(ctx context.Context, tenant string, actorIDs []string) error
}
