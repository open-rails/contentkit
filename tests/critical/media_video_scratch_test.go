package critical_test

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// A video encode keeps each chunk's renditions in its item's tmp/ only until
// their rung is assembled: a released video's folder holds none, a killed
// encode leaves only scratch the bucket's lifecycle expires, and erasing an
// encoded video deletes everything under its folder.
func TestMediaVideoScratch(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=426x240:rate=12", "-t", "1", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create source: %v %s", err, out)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	clips := func(c *ck.Config) {
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{240, 144}}}},
		})
	}
	clip := func(t *testing.T, m *resilientMedia, title string) (media.RefBody, string) {
		t.Helper()
		admin := m.app.engine.Client()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title}}}}})
		must(t, err)
		ref := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}})
		must(t, err)
		return ref, layout.Prefix(ref.Kind, ref.Version)
	}
	// objects lists the folder from the bucket itself.
	objects := func(t *testing.T, m *resilientMedia, folder string) []string {
		t.Helper()
		var keys []string
		pages := awss3.NewListObjectsV2Paginator(m.storage.management, &awss3.ListObjectsV2Input{Bucket: &m.storage.bucket, Prefix: &folder})
		for pages.HasMorePages() {
			page, err := pages.NextPage(t.Context())
			must(t, err)
			for _, object := range page.Contents {
				keys = append(keys, aws.ToString(object.Key))
			}
		}
		slices.Sort(keys)
		return keys
	}
	// encoded waits until every run is complete: a run completes after it
	// deletes its chunks.
	encoded := func(t *testing.T, m *resilientMedia) map[string]bool {
		t.Helper()
		runs := pgx.Identifier{m.config.Schema, "encode_run"}.Sanitize()
		waitForWithin(t, 60*time.Second, func() bool {
			var total, open int
			must(t, m.pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE state != 'complete') FROM `+runs).Scan(&total, &open))
			return total > 0 && open == 0
		})
		rows, err := m.pool.Query(t.Context(), `SELECT value FROM `+pgx.Identifier{m.config.Schema, "encode_chunk"}.Sanitize()+`, jsonb_each_text(output)`)
		must(t, err)
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		chunks := map[string]bool{}
		for _, key := range keys {
			chunks[key] = true
		}
		return chunks
	}

	t.Run("CK-J11-S11_a_released_video_keeps_no_chunks", func(t *testing.T) {
		m := newResilientMedia(t, clips)
		admin := m.app.engine.Client()
		ref, folder := clip(t, m, "Released without chunks")
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		startVideoFailureWorker(t, m)
		awaitMediaReady(t, admin, ref)
		waitForWithin(t, 30*time.Second, func() bool {
			view, err := admin.GetContentVersion(t.Context(), ref.ID, ref.Version, ck.GetContentVersionQuery{})
			return err == nil && view.Live
		})
		chunks := encoded(t, m)
		if len(chunks) < 2 {
			t.Fatalf("the encode recorded %d chunk renditions; the fixture needs one per rung", len(chunks))
		}
		for _, key := range objects(t, m, folder) {
			if chunks[key] || strings.HasPrefix(key, folder+layout.AreaTmp+"/") {
				t.Errorf("the released video keeps %s", key)
			}
		}
	})

	t.Run("CK-J11-S12_a_killed_encode_leaves_only_scratch_tagged_to_expire", func(t *testing.T) {
		m := newResilientMedia(t, clips)
		// The first assembled rendition: its rung's chunks are all stored.
		hold := m.wire.pause(t, func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/internal/") &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "video/mp4") && !strings.HasSuffix(r.URL.Path, digestBytes(video))
		})
		child := m.start(t, func(c *mediaChildConfig) {
			c.VideoCodecs, c.VideoEncoder, c.Preset, c.TopPreset = []media.Codec{media.CodecH264}, "cpu", "ultrafast", "ultrafast"
		})
		_, folder := clip(t, m, "Killed mid-encode")
		select {
		case <-hold.arrived:
		case <-time.After(60 * time.Second):
			t.Fatal("the encode did not reach its first rung's assembly")
		}
		child.kill(t)
		scratch := 0
		for _, key := range objects(t, m, folder) {
			k, ok := layout.Parse(key)
			if !ok {
				t.Errorf("the killed encode left %s outside the layout", key)
				continue
			}
			if k.Area != layout.AreaTmp {
				continue
			}
			if strings.HasPrefix(k.Name, "w-") {
				scratch++
			}
			tags, err := m.storage.management.GetObjectTagging(t.Context(), &awss3.GetObjectTaggingInput{Bucket: &m.storage.bucket, Key: &key})
			must(t, err)
			temporary := false
			for _, tag := range tags.TagSet {
				temporary = temporary || aws.ToString(tag.Key) == "contentkit-temp" && aws.ToString(tag.Value) == "true"
			}
			if !temporary {
				t.Errorf("the killed encode left %s without the bucket's temporary tag", key)
			}
		}
		if scratch == 0 {
			t.Fatal("the killed encode left no chunk scratch in tmp/")
		}
	})

	t.Run("CK-J08-S12_erasing_an_encoded_video_empties_its_folder", func(t *testing.T) {
		m := newResilientMedia(t, clips)
		admin := m.app.engine.Client()
		ref, folder := clip(t, m, "Erased after encoding")
		startVideoFailureWorker(t, m)
		awaitMediaReady(t, admin, ref)
		encoded(t, m)
		// What a killed run and an older worker leave beside the item's files.
		for _, key := range []string{folder + "tmp/w-" + uuid.NewString() + "-0-v144-h264-0.mp4", folder + "tmp/" + uuid.NewString() + "-0-v144-h264-0.mp4"} {
			_, err := m.storage.management.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: &m.storage.bucket, Key: &key, Body: strings.NewReader("chunk")})
			must(t, err)
		}
		must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
		awaitPublicationJobsIdle(t, m.world, m.app)
		deleted, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
		must(t, err)
		if len(deleted) != 1 || deleted[0].DeletedAt == nil {
			t.Fatalf("the deleted clip has no deletion time: %+v", deleted)
		}
		m.clock.set(deleted[0].DeletedAt.Add(30*24*time.Hour + time.Microsecond))
		producer, err := river.NewClient(riverpgxv5.New(m.pool), &river.Config{Schema: m.schema + "_river"})
		must(t, err)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(m.app.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 60*time.Second, func() bool {
			current, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if current.State == rivertype.JobStateDiscarded || current.State == rivertype.JobStateCancelled {
				t.Fatalf("the purge did not finish erasing the clip: %+v", current.Errors)
			}
			return current.State == rivertype.JobStateCompleted
		})
		want := []string{folder + layout.ManifestName, folder + layout.AreaPublic + "/" + layout.FenceName}
		if got := objects(t, m, folder); !slices.Equal(got, want) {
			t.Fatalf("the erased clip's folder holds %v; want only its tombstone and fence %v", got, want)
		}
	})
}
