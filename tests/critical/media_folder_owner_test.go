package critical_test

import (
	"errors"
	"testing"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
)

// A version's media folder has one parent. A journal row naming another,
// whichever side of the owner it sorts on, refuses the folder until it is gone.
func TestMediaFolderOwner(t *testing.T) {
	m := newResilientMedia(t)
	client := m.app.engine.Client()
	ref := galleryRef(t, client, "Owned media folder")
	for range 3 {
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMeta, Meta: map[string]any{"revision": uuid.NewString()}}}})
		must(t, err)
	}
	target, err := m.config.Kinds.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := m.config.Kinds.Item(target)
	must(t, err)
	journal, err := mediastore.NewPGJournal(m.pool, m.schema, nil)
	must(t, err)
	manifests, err := mediastore.NewManifests(m.storage.store, m.config.Kinds, mediastore.ManifestOptions{Locker: mediastore.PGLocker(m.pool), Journal: journal})
	must(t, err)
	_, _, err = manifests.Get(t.Context(), target)
	must(t, err)
	table := pgx.Identifier{m.schema, "content_media_commits"}.Sanitize()
	for _, claim := range []struct {
		name, content string
		version       *string
	}{
		{"sorts_before", "0", target.ContentVersionID},
		{"sorts_after", "zzzz" + ref.ID, target.ContentVersionID},
		{"same_content_without_version", ref.ID, nil},
	} {
		t.Run(claim.name, func(t *testing.T) {
			operation := uuid.New()
			_, err := m.pool.Exec(t.Context(), `INSERT INTO `+table+` (tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state)
VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,'applied')`, target.TenantID, operation, target.ContentKind, claim.content, claim.version, item.Prefix(), make([]byte, 32), uuid.New())
			must(t, err)
			if _, _, err := manifests.Get(t.Context(), target); !errors.Is(err, mediastore.ErrCommitIdentity) {
				t.Fatalf("folder claimed by a second parent was read: %v", err)
			}
			_, err = m.pool.Exec(t.Context(), `DELETE FROM `+table+` WHERE tenant_id=$1 AND operation_id=$2`, target.TenantID, operation)
			must(t, err)
			_, _, err = manifests.Get(t.Context(), target)
			must(t, err)
		})
	}
}
