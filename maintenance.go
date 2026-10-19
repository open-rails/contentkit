package contentkit

import "context"

// ReconcileFacets repairs the catalog's listing facets, current access
// snapshots and search-document marks from canonical content and versions.
// It starts no workers and may run before Start. It is an explicit maintenance
// operation; it must not run on an engine opened for erasure recovery.
func (e *Engine) ReconcileFacets(ctx context.Context) (int, error) {
	if e.erasureRecovery {
		return 0, ErrErasureReplayRequired
	}
	if err := e.checkErasureReplay(ctx); err != nil {
		return 0, err
	}
	return e.catalog.ReconcileFacets(ctx)
}
