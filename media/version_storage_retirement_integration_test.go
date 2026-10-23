package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	riverhelpers "github.com/open-rails/helpers/river"
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
	if _, err := jobs.CleanupDue(t.Context(), 100); err != nil {
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
	if _, err := jobs.CleanupDue(t.Context(), 100); err == nil {
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
	if _, err := jobs.CleanupDue(t.Context(), 100); err != nil {
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
	if _, err := jobs.CleanupDue(t.Context(), 100); err != nil {
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

func TestVersionStorageMigrationForwardsPendingDeletion(t *testing.T) {
	t.Run("matching_incarnation", func(t *testing.T) { testVersionStorageForwardedDeletion(t, false) })
	t.Run("restored_incarnation", func(t *testing.T) { testVersionStorageForwardedDeletion(t, true) })
}

func testVersionStorageForwardedDeletion(t *testing.T, restore bool) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	before := f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	schema := migrationRiver(t, f)
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	due := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	args, _ := json.Marshal(map[string]any{"prefix": old, "owner": "owner", "incarnation": before.Incarnation})
	var oldID int64
	if err := f.env.Pool().QueryRow(t.Context(), `INSERT INTO `+table+` (kind,args,queue,max_attempts,state,scheduled_at)
VALUES('contentkit_media_delete_folder',$1,'contentkit_media',5,'running',$2) RETURNING id`, args, due).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err == nil {
		t.Fatal("migration changed an active old deletion")
	}
	var active int
	if err := f.env.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE state='running' AND id=$1`, oldID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("failed migration consumed running delete: %d %v", active, err)
	}
	if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+table+` SET state='scheduled' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err != nil {
		t.Fatal(err)
	}
	target, _ := f.reg.Item(ref)
	var newID int64
	var newArgs []byte
	var newDue time.Time
	var attempts int
	if err := f.env.Pool().QueryRow(t.Context(), `SELECT id,args,scheduled_at,max_attempts FROM `+table+`
WHERE kind='contentkit_media_delete_folder' AND args->>'prefix'=$1 AND state='scheduled'`, target.Prefix()).Scan(&newID, &newArgs, &newDue, &attempts); err != nil {
		t.Fatal(err)
	}
	var forwarded struct{ Prefix, Owner, Incarnation string }
	if err := json.Unmarshal(newArgs, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.Prefix != target.Prefix() || forwarded.Owner != "owner" || forwarded.Incarnation != before.Incarnation || !newDue.Equal(due) || attempts != 5 {
		t.Fatalf("pending deletion lost its original semantics: %+v %s attempts=%d", forwarded, newDue, attempts)
	}
	var oldState string
	if err := f.env.Pool().QueryRow(t.Context(), `SELECT state FROM `+table+` WHERE id=$1`, oldID).Scan(&oldState); err != nil || oldState != "cancelled" {
		t.Fatalf("old deletion stayed executable: %s %v", oldState, err)
	}
	if restore {
		// A forwarded old intent must not delete a newly restored incarnation.
		if err := f.jobs.Purge(t.Context(), media.Deletion{Ref: ref, Owner: "owner", OperationID: "restored-after-layout"}); err != nil {
			t.Fatal(err)
		}
		f.put(ref, "originals/restored.png", "image/png", png(2))
	}
	if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+table+` SET scheduled_at=now()-interval '1 minute' WHERE id=$1`, newID); err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store)})
	if err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(t.Context(), f.env.Pool(), &river.Config{Schema: schema,
		FetchPollInterval: 20 * time.Millisecond, FetchCooldown: 10 * time.Millisecond}, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.StopAndCancel(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for {
		job, err := client.JobGet(ctx, newID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == rivertype.JobStateCompleted {
			break
		}
		if job.State == rivertype.JobStateCancelled || job.State == rivertype.JobStateDiscarded {
			t.Fatalf("forwarded deletion did not complete: %s %+v", job.State, job.Errors)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("forwarded deletion stayed %s: %v", job.State, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	current, _, err := f.ms.Get(t.Context(), ref)
	if restore {
		if err != nil || current.Incarnation == before.Incarnation || current.Find("originals/restored.png") < 0 {
			t.Fatalf("old-incarnation deletion destroyed restored version: %+v %v", current, err)
		}
	} else if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("matching-incarnation delete did not tombstone the flat target: %+v %v", current, err)
	}
}
