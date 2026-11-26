package contentkit

import (
	"context"
	"errors"

	"github.com/open-rails/contentkit/media"

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
	// Releases: released versions that lack a required upload
	// (media.Upload.Required), whose release was woken to withdraw them.
	Releases int
}

// Reconcile repairs the catalog's derived state from canonical content,
// versions and entities: listing facets, current access snapshots,
// search-document marks and content paths. It also wakes the release of
// released versions that lack a required upload, which withdraws them; the
// process running StartJobs does that work. It starts no workers and may run
// before Start. It is an explicit
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
	out.Releases, err = e.versions.WakeRequiredReleases(ctx, client)
	return out, err
}

// RegenerateMedia asks the media worker to bring the items r selects up to
// date after a deploy changed a kind's master recipe or a rendition's spec:
// masters whose recipe changed are made again, then renditions whose master
// or spec changed. Each change is published as a new manifest revision; what
// it replaced is retired and collected. Current items write nothing, so it
// can be repeated, and a run cut short is resumed by running it again. It
// returns the items queued.
func (e *Engine) RegenerateMedia(ctx context.Context, r media.Regeneration) (int, error) {
	if e.media == nil {
		return 0, errors.New("contentkit: media is not configured")
	}
	return e.media.jobs.Regenerate(ctx, r)
}
