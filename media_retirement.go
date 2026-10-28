package contentkit

import (
	"context"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"
)

// catalogMedia preserves the catalog's existing deletion-sweep seam while the
// Engine also owns entity/poll/account folders in the same routed media store.
type catalogMedia struct {
	*versionstore.Service
	engine *Engine
}

func (m catalogMedia) ResumeDeletes(ctx context.Context, limit int) (int, error) {
	return m.engine.entities.PurgeFolders(ctx, limit)
}
