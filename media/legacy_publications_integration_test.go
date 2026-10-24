package media_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	riverhelpers "github.com/open-rails/helpers/river"
)

func TestLegacyPublicationRetirementPreservesVersionFiles(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Versions = true })
	source, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	f.put(source, "originals/001.png", "image/png", png(1))
	item, _ := f.reg.Item(source)
	root := strings.TrimSuffix(item.Prefix(), "gallery/"+cid(2)+"/")
	legacy := root + "gallery.published/" + cid(3) + "/"
	oldKey := legacy + "private/" + blobOf([]byte("legacy"))
	f.object(oldKey, "legacy")
	f.object(legacy+"manifest.json", "legacy manifest")
	operation := uuid.New()
	effects, _ := json.Marshal(map[string]any{"cleanup": true, "private": []string{oldKey, legacy + "manifest.json"}, "legacy_publication": map[string]any{
		"kind": "gallery", "content": cid(1), "version": cid(2), "source": root + "gallery/" + cid(1) + "/" + cid(2) + "/", "initialized": false, "final": false}})
	pool := f.env.Pool()
	commits := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+commits+` (tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES ($1,$2,'gallery.published',$3,$4,'migration:reader-metadata',$5,$6,'applied',$7)`, source.TenantID, operation, cid(3), legacy, make([]byte, 32), uuid.New(), effects); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+cleanupTable(f)+` (tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
VALUES($1,$2,'gallery.published','infinity',$3)`, source.TenantID, legacy, []uuid.UUID{operation}); err != nil {
		t.Fatal(err)
	}
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	const retention = 48 * time.Hour
	if err := media.MigrateCleanup(t.Context(), pool, f.reg, f.journal, schema, retention); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(47 * time.Hour)
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if !f.exists(oldKey) {
		t.Fatal("legacy copy deleted before old-reader retention")
	}
	clock = time.Now().Add(49 * time.Hour)
	current, _, err := f.ms.Get(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, _ := item.Blob(current.Files[0].Blob)
	badTargets, _ := json.Marshal([]string{oldKey, sourceKey})
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=jsonb_set(effects,'{private}',$2::jsonb) WHERE operation_id=$1`, operation, badTargets); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err == nil {
		t.Fatal("legacy receipt was allowed to target a source prefix")
	}
	if !f.exists(sourceKey) || !f.exists(oldKey) {
		t.Fatal("invalid legacy receipt deleted files")
	}
	validTargets, _ := json.Marshal([]string{oldKey, legacy + "manifest.json"})
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=jsonb_set(effects,'{private}',$2::jsonb) WHERE operation_id=$1`, operation, validTargets); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=jsonb_set(effects,'{mirror}',jsonb_build_array(jsonb_build_object('from',$2::text,'to',$3::text))) WHERE operation_id=$1`, operation, oldKey, sourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+cleanupTable(f)+` SET due_at=now() WHERE folder_prefix=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err == nil {
		t.Fatal("legacy receipt could overwrite the source through a mirror")
	}
	if !f.exists(oldKey) || !f.exists(sourceKey) {
		t.Fatal("invalid mirror receipt changed files")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+commits+` SET effects=effects-'mirror' WHERE operation_id=$1`, operation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+cleanupTable(f)+` SET due_at=now() WHERE folder_prefix=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	// A legitimate newer journaled adoption fences this old retirement,
	// even if the original retired manifest is unreadable.
	adopted := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+commits+` (tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects,created_at)
        VALUES($1,$2,'gallery.published',$3,$4,'restore',$5,$6,'applied','{}',now()+interval '1 second')`, source.TenantID, adopted, cid(3), legacy, make([]byte, 32), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err == nil {
		t.Fatal("retirement crossed a newer adoption")
	}
	if !f.exists(oldKey) || !f.exists(sourceKey) {
		t.Fatal("retirement deleted a restored owner's bytes")
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM `+commits+` WHERE operation_id=$1`, adopted); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE `+cleanupTable(f)+` SET due_at=now() WHERE folder_prefix=$1`, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if f.exists(oldKey) || f.exists(legacy+"manifest.json") {
		t.Fatal("retired copied folder survived")
	}
	current, _, err = f.ms.Get(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range current.Files {
		if file.Blob != "" {
			key, _ := item.Blob(file.Blob)
			if !f.exists(key) {
				t.Fatalf("retirement touched canonical file %s", key)
			}
		}
	}
	// A late old copy lands after the first cleanup; the same receipt removes it.
	f.object(oldKey, "late")
	clock = time.Now().Add(10 * 24 * time.Hour)
	if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if f.exists(oldKey) {
		t.Fatal("late legacy copy survived final pass")
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+cleanupTable(f)+" WHERE folder_prefix=$1", legacy).Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy obligation did not finish: %d %v", n, err)
	}
}

func TestLegacyRetiredPrefixesMigrateWithoutPublicationRows(t *testing.T) {
	for _, mode := range []string{"first_delete", "sweep_only", "final_delete"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Versions = true })
			source, err := f.reg.VersionRef("gallery", cid(1), cid(2))
			if err != nil {
				t.Fatal(err)
			}
			f.visible(1)
			f.put(source, "originals/001.png", "image/png", png(1))
			item, _ := f.reg.Item(source)
			root := strings.TrimSuffix(item.Prefix(), "gallery/"+cid(2)+"/")
			legacy := root + "gallery.published/" + cid(3) + "/"
			key := legacy + "private/" + blobOf([]byte(mode))
			f.object(key, mode)
			f.object(legacy+"manifest.json", "old")
			pool := f.env.Pool()
			commits := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
			prior := uuid.New()
			if _, err := pool.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize()+`(tenant_id,folder_prefix,object_key,incarnation) VALUES($1,$2,$3,$4)`, source.TenantID, legacy, key, uuid.New()); err != nil {
				t.Fatal(err)
			}
			if mode != "first_delete" {
				effects := map[string]any{}
				if mode == "final_delete" {
					effects = map[string]any{"deletion": true, "private": []string{key, legacy + "manifest.json"}}
				}
				body, _ := json.Marshal(effects)
				if _, err := pool.Exec(t.Context(), `INSERT INTO `+commits+`(tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,'gallery.published',$3,$4,'old-worker',$5,$6,'applied',$7)`, source.TenantID, prior, cid(3), legacy, make([]byte, 32), uuid.New(), body); err != nil {
					t.Fatal(err)
				}
			}
			schema := pgtest.EmptySchema(t, t.Context(), pool)
			if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
				t.Fatal(err)
			}
			var job int64
			if mode == "sweep_only" {
				if _, err := pool.Exec(t.Context(), `INSERT INTO `+cleanupTable(f)+`(tenant_id,folder_prefix,content_kind,due_at) VALUES($1,$2,'gallery.published',now())`, source.TenantID, legacy); err != nil {
					t.Fatal(err)
				}
			} else {
				args := map[string]any{"prefix": legacy, "incarnation": uuid.NewString()}
				if mode == "final_delete" {
					args["final"] = true
					args["operation"] = prior.String()
				}
				body, _ := json.Marshal(args)
				if err := pool.QueryRow(t.Context(), `INSERT INTO `+pgx.Identifier{schema, "river_job"}.Sanitize()+`(kind,args,queue,max_attempts,state) VALUES('contentkit_media_delete_folder',$1,'contentkit_media',3,'scheduled') RETURNING id`, body).Scan(&job); err != nil {
					t.Fatal(err)
				}
			}
			if err := media.MigrateCleanup(t.Context(), pool, f.reg, f.journal, schema, 24*time.Hour); err != nil {
				t.Fatal(err)
			}
			if job > 0 {
				var state string
				if err := pool.QueryRow(t.Context(), `SELECT state FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()+` WHERE id=$1`, job).Scan(&state); err != nil || state != "cancelled" {
					t.Fatalf("old timer not transferred: %s %v", state, err)
				}
			}
			jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store), Now: func() time.Time { return time.Now().Add(26 * time.Hour) }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
				t.Fatal(err)
			}
			if f.exists(key) {
				t.Fatal("retired legacy destination survived")
			}
			current, _, err := f.ms.Get(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range current.Files {
				if file.Blob != "" {
					sourceKey, _ := item.Blob(file.Blob)
					if !f.exists(sourceKey) {
						t.Fatal(fmt.Sprintf("source deleted in %s: %s", mode, sourceKey))
					}
				}
			}
		})
	}
}
