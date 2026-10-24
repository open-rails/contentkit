package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
)

type interruptedArchiveStore struct {
	media.Store
	pages int
}

func (s *interruptedArchiveStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts media.PutOptions) (media.Object, error) {
	if strings.Contains(key, "/private/") || strings.Contains(key, "/download/") {
		s.pages++
		if s.pages > 8 {
			return media.Object{}, media.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, opts)
}

func TestArchiveCheckpointRetryAndConcurrentOwnerEdits(t *testing.T) {
	for _, change := range []string{"restart", "save", "replace", "delete"} {
		t.Run(change, func(t *testing.T) {
			f, ledger := archiveFixture(t, 16<<20)
			f.visible(1)
			ref := f.ref("post", 1)
			ctx := f.workerContext(t.Context())
			files := map[string][]byte{}
			for i := 0; i < 25; i++ {
				files[fmt.Sprintf("page%02d.png", i)] = archivePNG(t, uint8(i))
			}
			source := archiveBytes(t, files)
			p, blob := f.upload(ref, "zip/book", "application/zip", source)
			f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
			interrupted := s3test.Manifests(t, &interruptedArchiveStore{Store: f.env.Store}, f.reg, media.ManifestOptions{Journal: f.journal})
			if err := interrupted.IngestArchives(ctx, ref, ""); !errors.Is(err, media.ErrUnavailable) {
				t.Fatalf("interrupted batch: %v", err)
			}
			status, err := f.rd.Status(t.Context(), ref, f.editor)
			if err != nil || status.Archives.Pages == nil || status.Archives.Pages.Completed != 8 || status.Archives.Pages.Total != 25 || status.Archives.Completed != 0 {
				t.Fatalf("committed checkpoint status: %+v %v", status, err)
			}
			events, err := eventpg.New(f.env.Pool(), eventpg.Config{Schema: f.env.ContentSchema(), Source: "contentkit", Scope: ref.TenantID})
			if err != nil {
				t.Fatal(err)
			}
			page, err := events.ListEvents(t.Context(), eventlog.Query{Types: []string{media.EventProcessingProgressed}})
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, event := range page.Events {
				var fact media.ProcessingEvent
				if err := json.Unmarshal(event.Data, &fact); err != nil {
					t.Fatal(err)
				}
				seen = seen || fact.Archives.Pages != nil && fact.Archives.Pages.Completed == 8
			}
			if !seen {
				t.Fatal("running ingest did not append its committed page progress")
			}
			switch change {
			case "save":
				f.commit(ref, media.Op{Op: media.OpAttach, Path: p})
			case "replace":
				replacement := archiveBytes(t, map[string][]byte{"replacement.png": archivePNG(t, 99)})
				path, upload := f.upload(ref, p, "application/zip", replacement)
				f.commit(ref, media.Op{Op: media.OpPut, Path: path, Blob: upload, Unattached: true, Replace: true})
			case "delete":
				if err := pgx.BeginFunc(t.Context(), f.env.Pool(), func(tx pgx.Tx) error { return f.jobs.RetireItemsTx(t.Context(), tx, media.Deletion{Ref: ref}) }); err != nil {
					t.Fatal(err)
				}
				if _, err := f.jobs.PurgeRetired(t.Context(), 100); err != nil {
					t.Fatal(err)
				}
			}
			restarted := s3test.Manifests(t, f.env.Store, f.reg, media.ManifestOptions{Journal: f.journal})
			for range 2 {
				if err := restarted.IngestArchives(ctx, ref, ""); err != nil {
					t.Fatal(err)
				}
			}
			m, _, err := restarted.Get(t.Context(), ref)
			if change == "delete" {
				if !errors.Is(err, media.ErrNotFound) {
					t.Fatalf("old ingest revived deleted item: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 25
			if change == "replace" {
				want = 1
			}
			count := 0
			identities := map[string]bool{}
			for _, file := range m.Files {
				if !file.IsUpload() || !strings.HasPrefix(file.Path, "inline/") {
					continue
				}
				count++
				if identities[file.CreateID] {
					t.Fatal("retry duplicated a page identity")
				}
				identities[file.CreateID] = true
				if change == "save" && file.Unattached {
					t.Fatal("later ingest lost Save attachment state")
				}
			}
			if count != want {
				t.Fatalf("retained %d pages, want %d", count, want)
			}
			used, pending, err := ledger.Usage(t.Context(), ref.TenantID, "owner")
			if err != nil || used != m.UploadBytes() || pending != 0 {
				t.Fatalf("retry quota=%d/%d expected=%d: %v", used, pending, m.UploadBytes(), err)
			}
		})
	}
}

func TestContentPurgeOwnsFolderFailureAndLateWrites(t *testing.T) {
	f, ledger := archiveFixture(t, 16<<20)
	f.visible(1)
	ref := f.ref("post", 1)
	f.put(ref, "inline/one", "image/png", archivePNG(t, 1))
	item, _ := f.reg.Item(ref)
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := item.Blob(m.Files[0].Blob)
	store := &eraseStore{Store: f.env.Store}
	store.fail.Store(true)
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Processes: f.q, Limiter: ledger})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), f.env.Pool(), func(tx pgx.Tx) error {
		return jobs.RetireItemsTx(t.Context(), tx, media.Deletion{Ref: ref, Owner: "owner"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(t.Context(), 100); err == nil {
		t.Fatal("storage failure completed deletion")
	}
	if done, err := jobs.RetirementComplete(t.Context(), ref); err != nil || done {
		t.Fatalf("failed storage lost retry intent: %v %v", done, err)
	}
	if !f.exists(key) {
		t.Fatal("failed delete lost source")
	}
	store.fail.Store(false)
	for range 2 {
		if _, err := jobs.PurgeRetired(t.Context(), 100); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := jobs.RetirementComplete(t.Context(), ref); err != nil || !done {
		t.Fatalf("successful storage did not finish intent: %v %v", done, err)
	}
	used, _, err := ledger.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || used != 0 {
		t.Fatalf("deletion quota %d: %v", used, err)
	}
	f.object(key, "late pre-retirement upload")
	if _, err := f.later().PurgeRetired(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if f.exists(key) {
		t.Fatal("content purge lost exact late-write cleanup")
	}
	var removed int
	if err := f.env.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.q.Schema(), "river_job"}.Sanitize()+` WHERE kind IN ('contentkit_media_recover','contentkit_media_cleanup_due','contentkit_media_delete_folder')`).Scan(&removed); err != nil || removed != 0 {
		t.Fatalf("retirement created %d removed jobs: %v", removed, err)
	}
}
