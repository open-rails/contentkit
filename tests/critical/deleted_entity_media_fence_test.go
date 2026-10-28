package critical_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// fenceFaults is the real store; an armed fault refuses the next fence write
// or removal before it reaches S3.
type fenceFaults struct {
	*medias3.Store
	put, remove atomic.Bool
}

func (s *fenceFaults) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if strings.HasSuffix(key, "/"+layout.FenceName) && s.put.Swap(false) {
		return media.Object{}, errors.New("test: fence write failed")
	}
	return s.Store.Put(ctx, key, body, size, o)
}

func (s *fenceFaults) Delete(ctx context.Context, key string) error {
	if strings.HasSuffix(key, "/"+layout.FenceName) && s.remove.Swap(false) {
		return errors.New("test: fence removal failed")
	}
	return s.Store.Delete(ctx, key)
}

// The gateway fence for entities: a deleted channel's or creator's avatar and
// cover answer 404 to everyone once its deletion request returns, through the
// API or an import, and a restore reopens them. A fence failure is reported
// and the caller's retry completes it. Bytes stay in place until purge, which
// erases them behind a fence no restore lifts.
func TestDeletedEntityMediaFence(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Content.EntityKinds["creator"] = kinds.EntityKind{Route: "creators", Avatar: true}
	})
	a := w.apps[0]
	jobs, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	m := attachMedia(t, w, a)
	faults := &fenceFaults{Store: m.store}
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Store = faults
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	must(t, a.engine.StartJobs(jobs))
	admin := a.engine.Client()

	root := layout.Scope{TenantID: a.cfg.Scope.TenantID.String(), AppID: a.cfg.Scope.AppID.String()}.Root()
	folder := func(kind, id string) string { return root + kind + "/" + id + "/" }
	// served lists an item's public/ and private/ keys, without its fence.
	served := func(t *testing.T, kind, id string) []string {
		t.Helper()
		var keys []string
		for _, area := range []string{layout.AreaPublic, layout.AreaPrivate} {
			out, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(folder(kind, id) + area + "/")})
			must(t, err)
			for _, object := range out.Contents {
				if k := aws.ToString(object.Key); !strings.HasSuffix(k, "/"+layout.FenceName) {
					keys = append(keys, k)
				}
			}
		}
		slices.Sort(keys)
		return keys
	}
	fence := func(t *testing.T, kind, id string) (string, bool) {
		t.Helper()
		out, err := m.management.HeadObject(t.Context(), &awss3.HeadObjectInput{Bucket: &m.bucket, Key: aws.String(folder(kind, id) + "public/" + layout.FenceName)})
		if err != nil {
			return "", false
		}
		return out.Metadata[layout.FenceMeta], true
	}
	expectFence := func(t *testing.T, kind, id, want string) {
		t.Helper()
		got, ok := fence(t, kind, id)
		if want == "" && ok || want != "" && (!ok || got != want) {
			t.Fatalf("%s %s has fence %q (present %v), want %q", kind, id, got, ok, want)
		}
	}
	expect := func(t *testing.T, want int, addresses ...string) {
		t.Helper()
		for _, address := range addresses {
			response, err := http.Get(address)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != want {
				t.Fatalf("%s returned HTTP %d, want %d", address, response.StatusCode, want)
			}
		}
	}
	// pictures uploads an avatar and a cover to an entity's item and returns
	// their image names once processed.
	pictures := func(t *testing.T, kind, id string) (avatar, cover string) {
		t.Helper()
		ref := media.RefBody{Kind: kind, ID: id}
		_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
			{Path: "avatar/face.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
			{Path: "cover/banner.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
		})
		must(t, err)
		ready := errors.New("pictures ready")
		watch, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		err = admin.WatchMedia(watch, ref, "", func(update ck.MediaProgress) error {
			if update.Read == nil || update.Read.Status == nil {
				return nil
			}
			if status := update.Read.Status; status.FailureCount > 0 {
				return fmt.Errorf("processing failed: %+v", status.Failures)
			} else if status.State == media.StateReady && status.Uploads.Completed == 2 {
				return ready
			}
			return nil
		})
		if !errors.Is(err, ready) {
			t.Fatalf("await %s %s pictures: %v", kind, id, err)
		}
		// The editor read renders its missing views; settle them so the item's
		// bytes stay put from here on.
		var read media.ReadResult
		waitForWithin(t, 30*time.Second, func() bool {
			read, err = admin.GetMedia(t.Context(), kind, id, ck.GetMediaQuery{Editor: true})
			must(t, err)
			return len(read.Files) == 2 && !slices.ContainsFunc(read.Files, func(f media.FileInfo) bool { return f.EditorURL == "" })
		})
		// An image's name is its upload's, without the extension.
		for _, file := range read.Files {
			name := strings.TrimSuffix(file.Path, path.Ext(file.Path))
			if name, ok := strings.CutPrefix(name, "avatar/"); ok {
				avatar = name
			} else if name, ok := strings.CutPrefix(name, "cover/"); ok {
				cover = name
			}
		}
		if avatar == "" || cover == "" {
			t.Fatalf("%s %s lacks processed pictures: %+v", kind, id, read.Files)
		}
		return avatar, cover
	}
	// urls are every rendition of an entity's avatar and cover.
	urls := func(t *testing.T, avatar *entities.Avatar, cover *entities.Cover) []string {
		t.Helper()
		var out []string
		if avatar != nil {
			for _, r := range avatar.Renditions {
				out = append(out, r.URL)
			}
		}
		n := len(out)
		if cover != nil {
			for _, r := range cover.Renditions {
				out = append(out, r.URL)
			}
		}
		if n == 0 || len(out) == n {
			t.Fatalf("entity lacks avatar or cover renditions: %+v %+v", avatar, cover)
		}
		return out
	}

	_, owner, channel := ownedChannel(t, w, a, "Fenced channel")
	avatar, cover := pictures(t, "channel", channel.ID)
	_, err = admin.PatchChannel(t.Context(), channel.ID, channels.Update{Avatar: &entities.Avatar{Image: avatar}, Cover: &entities.Cover{Image: cover}})
	must(t, err)
	read, err := a.client("").GetChannel(t.Context(), channel.ID, ck.GetChannelQuery{})
	must(t, err)
	channelPictures := urls(t, read.Avatar, read.Cover)
	channelBytes := served(t, "channel", channel.ID)

	made, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "creator", Slug: "fenced-creator", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Fenced creator"}}}})
	must(t, err)
	creator := string(made[0].ID)
	avatar, cover = pictures(t, "creator", creator)
	_, err = admin.PatchEntity(t.Context(), creator, entities.EntityUpdate{Avatar: &entities.Avatar{Image: avatar}, Cover: &entities.Cover{Image: cover}})
	must(t, err)
	detail, err := a.client("").GetEntity(t.Context(), creator, ck.GetEntityQuery{})
	must(t, err)
	creatorPictures := urls(t, detail.Avatar, detail.Cover)
	creatorBytes := served(t, "creator", creator)

	t.Run("CK-J16-S09_deleting_an_entity_closes_its_avatar_and_cover_at_once", func(t *testing.T) {
		expect(t, http.StatusOK, append(slices.Clone(channelPictures), creatorPictures...)...)
		must(t, a.client(owner).DeleteChannel(t.Context(), channel.ID))
		expect(t, http.StatusNotFound, channelPictures...)
		expect(t, http.StatusOK, creatorPictures...)
		expectFence(t, "channel", channel.ID, layout.FenceDeleted)
		if after := served(t, "channel", channel.ID); !slices.Equal(after, channelBytes) {
			t.Fatalf("deletion moved served bytes: %v, was %v", after, channelBytes)
		}
		// A failed fence is reported after the deletion commits; the retry
		// completes it.
		faults.put.Store(true)
		if _, err := admin.DeleteEntity(t.Context(), creator); err == nil {
			t.Fatal("a failed fence reported success")
		}
		_, err := a.client("").GetEntity(t.Context(), creator, ck.GetEntityQuery{})
		requireError(t, err, 404, "not_found")
		_, err = admin.DeleteEntity(t.Context(), creator)
		must(t, err)
		expect(t, http.StatusNotFound, creatorPictures...)
		expectFence(t, "creator", creator, layout.FenceDeleted)
	})
	t.Run("CK-J16-S10_restoring_an_entity_reopens_its_media_in_place", func(t *testing.T) {
		active := entities.StateActive
		_, err := admin.PatchChannel(t.Context(), channel.ID, channels.Update{State: &active})
		must(t, err)
		expect(t, http.StatusOK, channelPictures...)
		expectFence(t, "channel", channel.ID, "")
		faults.remove.Store(true)
		if _, err := admin.PatchEntity(t.Context(), creator, entities.EntityUpdate{State: &active}); err == nil {
			t.Fatal("a failed fence removal reported success")
		}
		_, err = admin.PatchEntity(t.Context(), creator, entities.EntityUpdate{State: &active})
		must(t, err)
		expect(t, http.StatusOK, creatorPictures...)
		expectFence(t, "creator", creator, "")
		if after := served(t, "channel", channel.ID); !slices.Equal(after, channelBytes) {
			t.Fatalf("restore moved served bytes: %v, was %v", after, channelBytes)
		}
		if after := served(t, "creator", creator); !slices.Equal(after, creatorBytes) {
			t.Fatalf("restore moved served bytes: %v, was %v", after, creatorBytes)
		}
	})
	t.Run("CK-J16-S11_imported_deletion_and_restore_close_and_reopen_media", func(t *testing.T) {
		record := importer.EntityRecord{ExternalID: "artist", Kind: "creator", Slug: "imported-artist", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Imported artist"}}}
		batch := importer.Batch{Source: "entity-media-fence", Entities: []importer.EntityRecord{record}}
		imported := func(batch importer.Batch, section string) importer.Result {
			t.Helper()
			result, err := admin.PostImport(t.Context(), batch)
			must(t, err)
			if len(result.Failed) != 0 || result.Written[section] != 1 {
				t.Fatalf("import failed: %+v", result)
			}
			return result
		}
		artist := imported(batch, importer.SectionEntities).IDs[importer.RecordEntity][record.ExternalID]
		if artist == "" {
			t.Fatal("import did not return the artist's id")
		}
		batch.Entities[0].Avatar, batch.Entities[0].CoverImage = pictures(t, "creator", artist)
		imported(batch, importer.SectionEntities)
		detail, err := a.client("").GetEntity(t.Context(), artist, ck.GetEntityQuery{})
		must(t, err)
		artistPictures := urls(t, detail.Avatar, detail.Cover)
		expect(t, http.StatusOK, artistPictures...)

		imported(importer.Batch{Source: batch.Source, Deletes: []importer.DeleteRecord{{Entity: importer.RecordEntity, ExternalID: record.ExternalID}}}, importer.SectionDeletes)
		expect(t, http.StatusNotFound, artistPictures...)
		expectFence(t, "creator", artist, layout.FenceDeleted)
		imported(batch, importer.SectionEntities)
		expect(t, http.StatusOK, artistPictures...)
		expectFence(t, "creator", artist, "")
		batch.Entities[0].State = string(entities.StateDeleted)
		imported(batch, importer.SectionEntities)
		expect(t, http.StatusNotFound, artistPictures...)
		expectFence(t, "creator", artist, layout.FenceDeleted)
	})
	t.Run("CK-J16-S12_purge_erases_entity_media_and_its_fence_outlives_restore", func(t *testing.T) {
		must(t, admin.DeleteChannel(t.Context(), channel.ID))
		expect(t, http.StatusNotFound, channelPictures...)
		entitiesTable := pgx.Identifier{w.schema, "content_entities"}.Sanitize()
		// Entity retention runs on the wall clock: its deletion is a day overdue.
		_, err := w.pool.Exec(t.Context(), "UPDATE "+entitiesTable+" SET deleted_at=$3 WHERE tenant_id=$1 AND id=$2",
			a.cfg.Scope.AppID.String(), channel.ID, time.Now().Add(-entities.DefaultDeletedRetention-24*time.Hour))
		must(t, err)
		producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
		must(t, err)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 45*time.Second, func() bool {
			row, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if row.State == rivertype.JobStateDiscarded {
				t.Fatalf("purge failed: %+v", row.Errors)
			}
			return row.State == rivertype.JobStateCompleted
		})
		var purged bool
		must(t, w.pool.QueryRow(t.Context(), "SELECT media_purged_at IS NOT NULL FROM "+entitiesTable+" WHERE tenant_id=$1 AND id=$2",
			a.cfg.Scope.AppID.String(), channel.ID).Scan(&purged))
		if !purged {
			t.Fatal("purge did not complete the channel's media retirement")
		}
		if left := served(t, "channel", channel.ID); len(left) != 0 {
			t.Fatalf("purge left served files: %v", left)
		}
		expectFence(t, "channel", channel.ID, layout.FenceErased)
		active := entities.StateActive
		_, err = admin.PatchChannel(t.Context(), channel.ID, channels.Update{State: &active})
		requireError(t, err, 409, "conflict")
		expect(t, http.StatusNotFound, channelPictures...)
		// Even a stale restore's fence sync leaves the erasure in place.
		config, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
		must(t, err)
		journal, err := mediastore.NewPGJournal(w.pool, w.schema, nil)
		must(t, err)
		storage, err := mediastore.NewJobs(mediastore.JobsConfig{Store: m.store, Registry: registry, Locker: mediastore.PGLocker(w.pool), Journal: journal})
		must(t, err)
		ref, err := registry.Ref("channel", channel.ID)
		must(t, err)
		must(t, storage.FenceDeleted(t.Context(), ref, false))
		expectFence(t, "channel", channel.ID, layout.FenceErased)
		expect(t, http.StatusNotFound, channelPictures...)
	})
}
