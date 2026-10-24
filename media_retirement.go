package contentkit

import (
	"context"
	"github.com/open-rails/contentkit/media/versions"
)

// catalogMedia preserves the catalog's existing deletion-sweep seam while the
// Engine also owns entity/poll/account folders in the same routed media store.
type catalogMedia struct {
	*versions.Service
	engine *Engine
}

func (m catalogMedia) ResumeDeletes(ctx context.Context, limit int) (int, error) {
	return m.engine.entities.PurgeFolders(ctx, limit)
}
