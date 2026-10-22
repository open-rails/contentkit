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
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	riverhelpers "github.com/open-rails/helpers/river"
)

func versionFixture(t *testing.T) *fixture {
	return newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Versions = true })
}

// Arrange the exact prior storage layout, including its immutable journal and
// allocation keys. Only this disposable fixture rewrites historical receipts.
func seedNestedVersion(t *testing.T, f *fixture, ref contentref.ContentRef) string {
	t.Helper()
	item, err := f.reg.Item(ref)
	if err != nil {
		t.Fatal(err)
	}
	flat := item.Prefix()
	root := strings.TrimSuffix(flat, ref.Version()+"/")
	nested := root + ref.ContentID + "/" + ref.Version() + "/"
	for object, err := range f.env.Store.List(t.Context(), flat) {
		if err != nil {
			t.Fatal(err)
		}
		to := nested + strings.TrimPrefix(object.Key, flat)
		if _, err := f.env.Store.Copy(t.Context(), object.Key, to, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := f.env.Store.Delete(t.Context(), object.Key); err != nil {
			t.Fatal(err)
		}
	}
	prefix := pgx.Identifier{f.env.ContentSchema()}.Sanitize() + "."
	for _, query := range []string{
		`UPDATE ` + prefix + `content_media_commits SET folder_prefix=$3,effects=replace(effects::text,$2,$3)::jsonb WHERE tenant_id=$1 AND folder_prefix=$2`,
		`UPDATE ` + prefix + `content_media_allocations SET folder_prefix=$3,object_key=replace(object_key,$2,$3) WHERE tenant_id=$1 AND folder_prefix=$2`,
		`UPDATE ` + prefix + `content_media_cleanup SET folder_prefix=$3 WHERE tenant_id=$1 AND folder_prefix=$2`,
	} {
		if _, err := f.env.Pool().Exec(t.Context(), query, ref.TenantID, flat, nested); err != nil {
			t.Fatal(err)
		}
	}
	return nested
}

func migrationRiver(t *testing.T, f *fixture) string {
	t.Helper()
	schema := pgtest.EmptySchema(t, t.Context(), f.env.Pool())
	if err := riverhelpers.ApplyMigrations(t.Context(), f.env.Pool(), schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestFlatVersionStorageKeepsDomainIdentity(t *testing.T) {
	f := versionFixture(t)
	parent, version := cid(10), cid(20)
	ref, err := f.reg.VersionRef("gallery", parent, version)
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.reg.Item(ref)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ContentID != parent || ref.Version() != version || !strings.HasSuffix(item.Prefix(), "/gallery/"+version+"/") || strings.Contains(item.Prefix(), parent) {
		t.Fatalf("domain/storage conflated: %v %s", ref, item.Prefix())
	}
	if _, err := f.reg.Item(contentref.New(ref.TenantID, "gallery", parent)); err == nil {
		t.Fatal("parent content got a media folder")
	}
	f.visible(10)
	f.put(ref, "originals/one.png", "image/png", png(1))
	wrong, err := f.reg.VersionRef("gallery", cid(11), version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.ms.Get(t.Context(), wrong); !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("wrong parent read: %v", err)
	}
	if _, err := f.ms.Edit(t.Context(), wrong, func(m *media.Manifest) error { m.Meta = map[string]any{"wrong": true}; return nil }); !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("wrong parent write: %v", err)
	}
	if _, err := f.jobs.Sweep(t.Context(), ref); err != nil {
		t.Fatalf("inverse folder owner: %v", err)
	}
	var wrongRows int
	query := `SELECT count(*) FROM ` + pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize() + ` WHERE tenant_id=$1 AND content_id=$2`
	if err := f.env.Pool().QueryRow(t.Context(), query, ref.TenantID, wrong.ContentID).Scan(&wrongRows); err != nil || wrongRows != 0 {
		t.Fatalf("wrong claim polluted journal: %d %v", wrongRows, err)
	}
}

func TestVersionStorageMigrationResumesAndRetiresOnlyOldAddress(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err == nil {
		t.Fatal("startup accepted an unmigrated nested folder")
	}
	schema := migrationRiver(t, f)
	interrupted, err := media.NewJobs(media.JobsConfig{Store: &stopCopyStore{Store: f.env.Store, left: 1}, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interrupted.MigrateVersionStorage(t.Context(), schema); err == nil {
		t.Fatal("copy interruption not reported")
	}
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err == nil {
		t.Fatal("startup accepted an incomplete checkpoint")
	}
	report, err := f.jobs.MigrateVersionStorage(t.Context(), schema)
	if err != nil {
		t.Fatal(err)
	}
	if report.Versions != 1 || report.Resumed != 1 || report.Copied != 1 || report.RetirementScheduled != 1 {
		t.Fatalf("resume report: %+v", report)
	}
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	source := m.Files[m.Find("originals/one.png")]
	newKey, _ := item.Blob(source.Blob)
	if !f.exists(newKey) || !f.exists(old+layout.AreaPrivate+"/"+source.Blob) {
		t.Fatal("verified migration did not preserve both copies before grace")
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store), Now: func() time.Time { return time.Now().Add(26 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.CleanupDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if !f.exists(newKey) {
		t.Fatal("old cleanup deleted flat source")
	}
	for object, err := range f.env.Store.List(t.Context(), old) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("old nested object survived: %s", object.Key)
	}
	if again, err := f.jobs.MigrateVersionStorage(context.Background(), schema); err != nil || again.Copied != 0 {
		t.Fatalf("completed migration replayed: %+v %v", again, err)
	}
}

func TestVersionStorageMigrationPreservesLogicalQuota(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := media.NewPGLimiter(f.env.Pool(), f.env.ContentSchema(), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	f.up, err = media.NewUploads(media.UploadOptions{Store: f.env.Store, Manifests: f.ms, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	before, _, err := limiter.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || before <= 0 {
		t.Fatalf("upload did not record logical quota: %d %v", before, err)
	}
	old := seedNestedVersion(t, f, ref)
	plan, err := media.PlanLayoutMigration(t.Context(), f.ms, f.reg, []media.LayoutMapping{{From: ref, To: ref, SourceFolder: old, Owner: "owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := media.ApplyLayoutMigration(t.Context(), f.ms, f.jobs, plan); err != nil {
		t.Fatal(err)
	}
	after, _, err := limiter.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || after != before {
		t.Fatalf("address change charged content twice: before %d after %d err %v", before, after, err)
	}
}

func TestVersionStorageMigrationRefusesUnexplainedAbsence(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	f.drop(old)
	schema := migrationRiver(t, f)
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err == nil || !strings.Contains(err.Error(), "committed source manifest is missing") {
		t.Fatalf("lost live media was silently considered absent: %v", err)
	}
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err == nil {
		t.Fatal("startup accepted lost live media")
	}
}

func TestVersionStorageMigrationPreservesErasureFence(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	if err := f.jobs.Erase(t.Context(), media.Deletion{Ref: ref, Owner: "owner", OperationID: "erase-before-layout-migration"}); err != nil {
		t.Fatal(err)
	}
	old := seedNestedVersion(t, f, ref)
	schema := migrationRiver(t, f)
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	if !f.exists(item.PublicPrefix()+layout.ErasureFenceName) || !f.exists(old+layout.AreaPublic+"/"+layout.ErasureFenceName) {
		t.Fatal("migration discarded permanent erasure fence")
	}
	if _, _, err := f.ms.Get(t.Context(), ref); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("migration resurrected erased version: %v", err)
	}
}

func TestVersionStorageMigrationRecordsNeverCommittedAbsence(t *testing.T) {
	f := versionFixture(t)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("failed before manifest write")
	if _, err := f.ms.Edit(t.Context(), ref, func(*media.Manifest) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	seedNestedVersion(t, f, ref)
	schema := migrationRiver(t, f)
	report, err := f.jobs.MigrateVersionStorage(t.Context(), schema)
	if err != nil || report.Absent != 1 || report.Copied != 0 {
		t.Fatalf("never-committed absence was not recorded: %+v %v", report, err)
	}
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err != nil {
		t.Fatal(err)
	}
	if report, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err != nil || report.Absent != 1 || report.Copied != 0 {
		t.Fatalf("absence checkpoint was not durable: %+v %v", report, err)
	}
}

func TestVersionStorageMigrationRequiresPurgedCatalogForDeletedAbsence(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	if err := f.jobs.Erase(t.Context(), media.Deletion{Ref: ref, Owner: "owner", OperationID: "already-purged-before-migration"}); err != nil {
		t.Fatal(err)
	}
	old := seedNestedVersion(t, f, ref)
	f.drop(old) // the historical version has already been fully removed
	schema := migrationRiver(t, f)
	jobs := f.later()
	if _, err := jobs.MigrateVersionStorage(t.Context(), schema); err == nil {
		t.Fatal("generic media inferred permanent catalog deletion")
	}
	opt := media.VersionStorageOptions{VersionPurged: func(_ context.Context, got contentref.ContentRef) (bool, error) {
		if !got.Equal(ref) {
			t.Fatalf("catalog proof asked about a different owner: %v", got)
		}
		return false, nil
	}}
	if _, err := jobs.MigrateVersionStorage(t.Context(), schema, opt); err == nil {
		t.Fatal("a restorable catalog version was treated as permanently absent")
	}
	opt.VersionPurged = func(_ context.Context, got contentref.ContentRef) (bool, error) { return got.Equal(ref), nil }
	if report, err := jobs.MigrateVersionStorage(t.Context(), schema, opt); err != nil || report.Absent != 1 {
		t.Fatalf("proven purged version blocked migration: %+v %v", report, err)
	}
	if err := media.ValidateVersionStorageMigration(t.Context(), f.reg, f.journal); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	if f.exists(item.ManifestKey()) {
		t.Fatal("proven purged version was resurrected")
	}
}

func TestVersionStorageRetirementRefusesSourceDriftAfterCopy(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	schema := migrationRiver(t, f)
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	args, _ := json.Marshal(map[string]any{"prefix": old})
	var id int64
	if err := f.env.Pool().QueryRow(t.Context(), `INSERT INTO `+table+`(kind,args,queue,max_attempts,state)
VALUES('contentkit_media_sweep',$1,'contentkit_media',5,'running') RETURNING id`, args).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); err == nil {
		t.Fatal("running old cleanup did not interrupt after copying")
	}
	item, _ := f.reg.Item(ref)
	current, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatalf("test did not reach copied checkpoint: %v", err)
	}
	changed := current.Clone()
	changed.Meta = map[string]any{"changed": "by an old writer after copying"}
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	f.object(old+layout.ManifestName, string(body))
	if _, err := f.env.Pool().Exec(t.Context(), `UPDATE `+table+` SET state='scheduled' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("newer source retired against stale copy: %v", err)
	}
	if !f.exists(old+layout.ManifestName) || !f.exists(item.ManifestKey()) {
		t.Fatal("failed drift check changed either retained manifest")
	}
}

func TestVersionStorageMigrationRefusesUnrelatedEmptyTarget(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	if _, err := f.ms.Create(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), migrationRiver(t, f)); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("migration replaced an unrelated empty target: %v", err)
	}
	if !f.exists(old + layout.ManifestName) {
		t.Fatal("refusal removed the original")
	}
}

func TestVersionStorageResumeRefusesLostAllocations(t *testing.T) {
	for _, change := range []string{"retired", "reassigned"} {
		t.Run(change, func(t *testing.T) {
			f := versionFixture(t)
			f.visible(1)
			ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
			if err != nil {
				t.Fatal(err)
			}
			f.put(ref, "originals/one.png", "image/png", png(1))
			f.produce(ref)
			old := seedNestedVersion(t, f, ref)
			schema := migrationRiver(t, f)
			interrupted, err := media.NewJobs(media.JobsConfig{Store: &stopCopyStore{Store: f.env.Store, left: 1}, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := interrupted.MigrateVersionStorage(t.Context(), schema); err == nil {
				t.Fatal("manifest write did not stop")
			}
			item, _ := f.reg.Item(ref)
			column := "retired_at=now()"
			if change == "reassigned" {
				column = "incarnation=gen_random_uuid()"
			}
			query := `UPDATE ` + pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize() + ` SET ` + column + ` WHERE tenant_id=$1 AND folder_prefix=$2`
			if tag, err := f.env.Pool().Exec(t.Context(), query, ref.TenantID, item.Prefix()); err != nil || tag.RowsAffected() == 0 {
				t.Fatalf("missing interrupted copy allocation: %v", err)
			}
			if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); !errors.Is(err, media.ErrAllocationRetired) {
				t.Fatalf("migration reused %s allocation: %v", change, err)
			}
			if !f.exists(old + layout.ManifestName) {
				t.Fatal("refusal removed source manifest")
			}
		})
	}
}

func TestVersionStorageResumeRejectsUnrelatedRecoveryFence(t *testing.T) {
	f := versionFixture(t)
	f.visible(1)
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.put(ref, "originals/one.png", "image/png", png(1))
	f.produce(ref)
	old := seedNestedVersion(t, f, ref)
	schema := migrationRiver(t, f)
	interrupted, err := media.NewJobs(media.JobsConfig{Store: &stopCopyStore{Store: f.env.Store, left: 1}, Registry: f.reg, Journal: f.journal, Locker: s3test.Locker(t, f.env.Store)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interrupted.MigrateVersionStorage(t.Context(), schema); err == nil {
		t.Fatal("manifest write did not stop")
	}
	if err := f.ms.Recover(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	unrelated := &media.Manifest{V: media.ManifestVersion, Hidden: true, Incarnation: uuid.NewString(), Files: []media.File{}}
	body, err := json.Marshal(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	f.object(item.ManifestKey(), string(body))
	if _, err := f.jobs.MigrateVersionStorage(t.Context(), schema); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("our absent attempt authorized another empty recovery fence: %v", err)
	}
	if !f.exists(old + layout.ManifestName) {
		t.Fatal("failed recovery fence proof removed the original")
	}
}
