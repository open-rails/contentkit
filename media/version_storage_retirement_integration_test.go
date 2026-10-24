package media_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func TestVersionStorageRetirementKeepsFlatSourceAndProjection(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Versions = true })
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	f.put(ref, "cover", "image/png", png(1))
	f.produce(ref)
	current, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := f.reg.Item(ref)
	root := strings.TrimSuffix(target.Prefix(), "gallery/"+cid(2)+"/")
	oldPrefix := root + "gallery/" + cid(1) + "/" + cid(2) + "/"
	oldKey := oldPrefix + "private/" + blobOf([]byte("old bytes"))
	f.object(oldKey, "old bytes")
	f.object(oldPrefix+"manifest.json", "old manifest")
	operation := uuid.New()
	effects, _ := json.Marshal(map[string]any{
		"cleanup": true, "private": []string{oldKey, oldPrefix + "manifest.json"},
		"version_storage": map[string]any{"target": target.Prefix(), "target_operation": current.Receipt.Operation, "final": false},
	})
	pool := f.env.Pool()
	commits := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
	publications := pgx.Identifier{f.env.ContentSchema(), "content_media_publications"}.Sanitize()
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+commits+`
(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,'gallery',$3,$4,$5,'migration:version-storage',$6,$7,'applied',$8)`,
		ref.TenantID, operation, ref.ContentID, ref.Version(), oldPrefix, make([]byte, 32), uuid.New(), effects); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+cleanupTable(f)+`(tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
VALUES($1,$2,'gallery',now()+interval '48 hours',$3)`, ref.TenantID, oldPrefix, []uuid.UUID{operation}); err != nil {
		t.Fatal(err)
	}
	projection := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY preset,upload_path),'[]')::text FROM `+publications+` p
WHERE tenant_id=$1 AND content_id=$2 AND content_version_id=$3`, ref.TenantID, ref.ContentID, ref.Version()).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := projection()
	clock := time.Now().Add(47 * time.Hour)
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Journal: f.journal,
		Locker: s3test.Locker(t, f.env.Store), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if !f.exists(oldKey) {
		t.Fatal("old issued URLs were retired before their retention deadline")
	}
	clock = time.Now().Add(49 * time.Hour)
	// A malformed receipt must never grant deletion of the canonical source.
	canonical, _ := target.Blob(current.Files[0].Blob)
	bad, _ := json.Marshal([]string{oldKey, canonical})
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=jsonb_set(effects,'{private}',$2::jsonb) WHERE operation_id=$1`, operation, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err == nil {
		t.Fatal("old-address receipt deleted a canonical target")
	}
	if !f.exists(oldKey) || !f.exists(canonical) {
		t.Fatal("invalid receipt changed storage")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=$2 WHERE operation_id=$1`, operation, effects); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+cleanupTable(f)+` SET due_at=now() WHERE folder_prefix=$1`, oldPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if f.exists(oldKey) || f.exists(oldPrefix+"manifest.json") || !f.exists(canonical) {
		t.Fatal("old-address retirement touched the live version or left old files")
	}
	if after := projection(); after != before {
		t.Fatal("old-address retirement changed the flat source's public projection")
	}
	// The retained exact targets catch a late old uploader without re-listing.
	f.object(oldKey, "late old bytes")
	clock = time.Now().Add(10 * 24 * time.Hour)
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if f.exists(oldKey) || !f.exists(canonical) {
		t.Fatal("late retirement missed the old key or touched canonical bytes")
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+cleanupTable(f)+` WHERE folder_prefix=$1`, oldPrefix).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old-address obligation did not finish: %d %v", count, err)
	}
}
