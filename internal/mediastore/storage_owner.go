package mediastore

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

func (j *PGJournal) resolveFolder(ctx context.Context, q folderQuerier, reg *Registry, prefix string) (Item, error) {
	if item, err := reg.folderItem(prefix); err == nil {
		return item, nil
	}
	key, ok := layout.ParseFolder(prefix)
	if !ok {
		return Item{}, fmt.Errorf("media: invalid storage folder %q", prefix)
	}
	kind, err := reg.Kind(key.Kind)
	if err != nil || !kind.Versions || key.Namespace != kind.ns {
		return Item{}, errors.New("media: folder does not belong to a versioned kind")
	}
	owner := key.ID
	if key.Version != "" || contentref.ValidateID(owner) != nil || prefix != kind.prefix(owner, "") {
		return Item{}, ErrCommitIdentity
	}
	jr := j.forTenant(kind.ns)
	rows, err := q.Query(ctx, `SELECT DISTINCT content_id,content_version_id FROM `+jr.table+`
WHERE tenant_id=$1 AND folder_prefix=$2 AND content_kind=$3 AND content_version_id=$4 LIMIT 2`, kind.ns, prefix, kind.Name, owner)
	if err != nil {
		return Item{}, err
	}
	type identity struct{ Content, Version string }
	identities, err := pgx.CollectRows(rows, pgx.RowToStructByPos[identity])
	if err != nil {
		return Item{}, err
	}
	if len(identities) != 1 {
		return Item{}, fmt.Errorf("%w: storage folder has no unique journal owner", ErrCommitIdentity)
	}
	ref, err := reg.VersionRef(kind.Name, identities[0].Content, identities[0].Version)
	if err != nil {
		return Item{}, err
	}
	item, err := reg.Item(ref)
	if err != nil {
		return Item{}, err
	}
	if item.Prefix() != prefix {
		return Item{}, ErrCommitIdentity
	}
	return item, nil
}

func (j *Jobs) itemForFolder(ctx context.Context, prefix string) (Item, error) {
	return j.cfg.Journal.resolveFolder(ctx, j.cfg.Journal.pool, j.cfg.Registry, prefix)
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
