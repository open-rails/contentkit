package media

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Folder paths contain a storage owner, not a parent content reference. The
// existing journal retains the full reference even after catalog deletion.
type folderQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (j *PGJournal) resolveFolder(ctx context.Context, q folderQuerier, reg *Registry, prefix string) (Item, bool, error) {
	if item, retired, err := reg.cleanupItem(prefix); err == nil {
		return item, retired, nil
	}
	key, ok := layout.ParseFolder(prefix)
	if !ok {
		return Item{}, false, fmt.Errorf("media: invalid storage folder %q", prefix)
	}
	kind, err := reg.Kind(key.Kind)
	if err != nil || !kind.Versions || key.Namespace != kind.ns {
		return Item{}, false, errors.New("media: folder does not belong to a versioned kind")
	}
	owner := key.ID
	if key.Version != "" {
		owner = key.Version
	}
	if contentref.ValidateID(owner) != nil || prefix != kind.nestedPrefix(key.ID, key.Version) {
		return Item{}, false, ErrCommitIdentity
	}
	jr := j.forTenant(kind.ns)
	rows, err := q.Query(ctx, `SELECT DISTINCT content_id,content_version_id FROM `+jr.table+`
WHERE tenant_id=$1 AND folder_prefix=$2 AND content_kind=$3 AND content_version_id=$4 LIMIT 2`, kind.ns, prefix, kind.Name, owner)
	if err != nil {
		return Item{}, false, err
	}
	type identity struct{ Content, Version string }
	identities, err := pgx.CollectRows(rows, pgx.RowToStructByPos[identity])
	if err != nil {
		return Item{}, false, err
	}
	if len(identities) != 1 {
		return Item{}, false, fmt.Errorf("%w: storage folder has no unique journal owner", ErrCommitIdentity)
	}
	ref, err := reg.VersionRef(kind.Name, identities[0].Content, identities[0].Version)
	if err != nil {
		return Item{}, false, err
	}
	item, err := reg.Item(ref)
	if err != nil {
		return Item{}, false, err
	}
	if key.Version != "" {
		// Historical jobs still refer to historical keys. Never redirect their
		// deletes to the live flat folder of the same logical version.
		if key.ID != ref.ContentID {
			return Item{}, false, ErrCommitIdentity
		}
		item.prefix = prefix
	} else if item.Prefix() != prefix {
		return Item{}, false, ErrCommitIdentity
	}
	return item, false, nil
}

func (j *Jobs) itemForFolder(ctx context.Context, prefix string) (Item, error) {
	item, _, err := j.cfg.Journal.resolveFolder(ctx, j.cfg.Journal.pool, j.cfg.Registry, prefix)
	if err != nil {
		return Item{}, err
	}
	canonical, err := j.cfg.Registry.Item(item.Ref())
	if err != nil {
		return Item{}, err
	}
	if canonical.Prefix() != item.Prefix() {
		return Item{}, fmt.Errorf("%w: historical nested folder needs explicit storage migration", ErrLayoutChanged)
	}
	return item, nil
}

// checkFolderOwner refuses a different parent claiming the same version-owned
// folder. Writers already hold the manifest lock; read calls require an
// existing claim. This check never runs in the media gateway.
func (j *PGJournal) checkFolderOwner(ctx context.Context, q folderQuerier, item Item, require bool) error {
	j = j.forTenant(item.Ref().TenantID)
	rows, err := q.Query(ctx, `SELECT DISTINCT content_kind,content_id,content_version_id FROM `+j.table+`
WHERE tenant_id=$1 AND folder_prefix=$2 LIMIT 2`, item.Ref().TenantID, item.Prefix())
	if err != nil {
		return err
	}
	type identity struct {
		Kind, Content string
		Version       *string
	}
	owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[identity])
	if err != nil {
		return err
	}
	if len(owners) == 0 && !require {
		return nil
	}
	if len(owners) != 1 {
		return ErrCommitIdentity
	}
	ref := contentref.New(item.Ref().TenantID, owners[0].Kind, owners[0].Content)
	ref.ContentVersionID = owners[0].Version
	if !ref.Equal(item.Ref()) {
		return ErrCommitIdentity
	}
	return nil
}
