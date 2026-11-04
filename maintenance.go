package contentkit

import (
	"context"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Reconciled counts the content items Reconcile repaired.
type Reconciled struct {
	// Facets: listing facets, current access snapshots and search-document
	// marks rebuilt.
	Facets int
	// Paths: decorative content paths re-derived from their path entities.
	Paths int
	// Releases: released versions whose live release lacks a required upload
	// (media.Upload.Required), queued for a release that withdraws them.
	Releases int
}

// Reconcile repairs the catalog's derived state from canonical content,
// versions and entities: listing facets, current access snapshots,
// search-document marks and content paths. It also queues the withdrawal of
// releases that lack a required upload; the process running StartJobs does
// that work. It starts no workers and may run before Start. It is an explicit
// maintenance operation; it must not run on an engine opened for erasure
// recovery.
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
	if out.Paths, err = e.catalog.RefreshPaths(ctx); err != nil {
		return out, err
	}
	client, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
	if err != nil {
		return out, err
	}
	out.Releases, err = e.versions.ReplanMissingUploads(ctx, client)
	return out, err
}
