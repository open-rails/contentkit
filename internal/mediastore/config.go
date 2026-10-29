package mediastore

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	media "github.com/open-rails/contentkit/media"
)

// findUpload is the index of the Upload in us whose Path is path, or -1.
func findUpload(us []media.Upload, path string) int {
	return slices.IndexFunc(us, func(u media.Upload) bool { return u.Path == path })
}

// findPrivate is the preset in ps named name, or nil.
func findPrivate(ps []media.Private, name string) *media.Private {
	if i := slices.IndexFunc(ps, func(p media.Private) bool { return p.Name == name }); i >= 0 {
		return &ps[i]
	}
	return nil
}

// spans reports aspects wider than lo and no wider than hi; unset is open.
func spans(lo, hi media.Aspect) bool { return lo.Native() || hi.Native() || wider(hi, lo) }

func wider(a, b media.Aspect) bool { return a.W*b.H > b.W*a.H }

// Hooks are the app's callbacks, given to NewRegistry beside its Config.
type Hooks struct {
	// Resolver authorizes media reads. Declared public presets do not depend
	// on publication visibility.
	Resolver resolve.Resolver
	// ReaderManifest supplies the catalog's completed reader snapshot. It is a
	// metadata projection only: the gateway still authorizes one item's folder.
	// Editors use the current authoring manifest without this projection.
	ReaderManifest func(ctx context.Context, ref contentref.ContentRef, actor access.Actor, current *Manifest) (*Manifest, error)
	// ValidatePathsChanged validates virtual-name mutations before the manifest
	// write while its lock is held. It must perform reads only.
	ValidatePathsChanged func(ctx context.Context, ref contentref.ContentRef, renames map[string]string, removed []string) error
	// PathsChanged applies accepted virtual-name changes in the journal's
	// settlement transaction. Failure retries the same recorded mapping.
	PathsChanged func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, renames map[string]string, removed []string) error
	// CanUpload says who may write which upload path.
	CanUpload media.UploadAuthorizer
	// ItemCommitted reports an item a commit changed, before its processing
	// runs. Idempotent; an error fails the commit.
	ItemCommitted func(ctx context.Context, ref contentref.ContentRef) error
	// Failed reports an upload a producer cannot process, where it runs.
	Failed func(ctx context.Context, ref contentref.ContentRef, path string, err error)
}
