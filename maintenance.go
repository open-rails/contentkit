package contentkit

import "context"

// Reconciled counts the content items Reconcile repaired.
type Reconciled struct {
	// Facets: listing facets, current access snapshots and search-document
	// marks rebuilt.
	Facets int
	// Paths: decorative content paths re-derived from their path entities.
	Paths int
}

// Reconcile repairs the catalog's derived state from canonical content,
// versions and entities: listing facets, current access snapshots,
// search-document marks and content paths. It starts no workers and may run
// before Start. It is an explicit maintenance operation; it must not run on
// an engine opened for erasure recovery.
func (e *Engine) Reconcile(ctx context.Context) (Reconciled, error) {
	var out Reconciled
	if e.erasureRecovery {
		return out, ErrErasureReplayRequired
	}
	if err := e.checkErasureReplay(ctx); err != nil {
		return out, err
	}
	var err error
	if out.Facets, err = e.catalog.ReconcileFacets(ctx); err != nil {
		return out, err
	}
	out.Paths, err = e.catalog.RefreshPaths(ctx)
	return out, err
}
