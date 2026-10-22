package contentkit

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

func TestMigrateVersionStorageWithoutVersionedMediaIsNoop(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Tenant: "site", Media: &MediaConfig{Config: media.Config{Kinds: []media.Kind{{Name: "image"}}}}},
	} {
		report, err := MigrateVersionStorage(t.Context(), nil, cfg, nil)
		if err != nil || report != (media.VersionStorageReport{}) {
			t.Fatalf("unconfigured migration needed storage: %+v %v", report, err)
		}
	}
}

func TestVersionStoragePurgedRequiresPermanentCatalogRemoval(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	const tenant = "catalog-app"
	parent, version := contentref.NewID(), contentref.NewID()
	items, versions := pgx.Identifier{schema, "content_items"}.Sanitize(), pgx.Identifier{schema, "content_versions"}.Sanitize()
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+items+`(tenant_id,id,kind) VALUES($1,$2,'gallery')`, tenant, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+versions+`(tenant_id,id,content_id,languages) VALUES($1,$2,$3,ARRAY['en'])`, tenant, version, parent); err != nil {
		t.Fatal(err)
	}
	e := &Engine{pool: pool, schema: schema, cfg: Config{Tenant: tenant, Content: kinds.Config{Kinds: map[string]kinds.Kind{"gallery": {}}}}}
	ref := contentref.NewVersion("storage-partition", "gallery", parent, version)
	for _, state := range []string{"draft", "deleted"} {
		if _, err := pool.Exec(t.Context(), `UPDATE `+versions+` SET state=$1 WHERE tenant_id=$2 AND id=$3`, state, tenant, version); err != nil {
			t.Fatal(err)
		}
		if purged, err := e.versionStoragePurged(t.Context(), ref); err != nil || purged {
			t.Fatalf("%s version is not permanently removed: %t %v", state, purged, err)
		}
	}
	wrong := ref
	wrong.ContentID = contentref.NewID()
	if _, err := e.versionStoragePurged(t.Context(), wrong); !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("wrong-parent proof was accepted: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM `+versions+` WHERE tenant_id=$1 AND id=$2`, tenant, version); err != nil {
		t.Fatal(err)
	}
	if purged, err := e.versionStoragePurged(t.Context(), ref); err != nil || !purged {
		t.Fatalf("permanently removed version not recognized: %t %v", purged, err)
	}
}
