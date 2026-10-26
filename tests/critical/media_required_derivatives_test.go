package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
)

// The same actual derivative worker handles PNG and MP4. A held S3 output
// proves that ready images or a partial video cannot complete publication.
func TestRequiredMixedDerivativesBeforeRelease(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Content.Kinds["mixed"] = kinds.Kind{Route: "mixed", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "mixed", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "images/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "low", From: "images/{name}", To: "low/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}, {Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
			Public:  []media.Public{{Name: "thumb", From: "images/{name}", To: "thumb-{name}-{w}.webp", Widths: []int{24}, Image: media.Image{Aspect: media.Square}}},
		})
	})
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=256x144:rate=12", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "1", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create real mixed-media source: %v %s", err, out)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	admin := m.app.engine.Client()
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "mixed", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "All required outputs"}}}}})
	must(t, err)
	ref := media.RefBody{Kind: "mixed", ID: item.ID, Version: item.Versions[0].ID}
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "images/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}, {Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	hold := m.wire.pause(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/future/") && strings.HasPrefix(r.Header.Get("Content-Type"), "video/mp4")
	})
	config := m.config
	config.VideoCodecs = []media.Codec{media.CodecH264}
	config.VideoEncoder = "cpu"
	config.Preset = "ultrafast"
	config.TempDir = t.TempDir()
	processor, err := worker.New(t.Context(), config)
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Run(run) }()
	t.Cleanup(func() {
		hold.unblock()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("worker did not stop")
		}
	})
	select {
	case <-hold.arrived:
	case <-time.After(60 * time.Second):
		t.Fatal("video output never reached the held S3 boundary")
	}
	editor, err := admin.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	must(t, err)
	imageReady := false
	for _, f := range editor.Files {
		if f.Path == "low/one.webp" {
			imageReady = true
		}
	}
	if !imageReady || editor.Status == nil || editor.Status.State == media.StateReady {
		t.Fatalf("fixture did not reach partial image/video processing: %+v", editor.Status)
	}
	_, err = m.app.client("").GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
	requireError(t, err, 404, "not_found")
	var active bool
	must(t, m.pool.QueryRow(t.Context(), `SELECT release_id IS NOT NULL FROM `+pgx.Identifier{m.schema, "content_versions"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`, m.app.cfg.Scope.AppID.String(), ref.Version).Scan(&active))
	if active {
		t.Fatal("partial derivatives activated a release")
	}
	hold.unblock()
	awaitMediaReady(t, admin, ref)
	awaitEditionPublication(t, m.app, admin, item.ID, ref.Version)
	visible, err := m.app.client("").GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
	must(t, err)
	if !visible.Decision.Allowed {
		t.Fatal("complete mixed-media release remained unavailable")
	}
	var bad int
	must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.config.Schema, "river_job"}.Sanitize()+` WHERE kind IN ('contentkit_media_image','contentkit_media_video')`).Scan(&bad))
	if bad != 0 {
		t.Fatalf("mixed media created %d retired format jobs", bad)
	}
	var derivatives int
	must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.config.Schema, "river_job"}.Sanitize()+` WHERE kind='contentkit_media_derivatives'`).Scan(&derivatives))
	if derivatives == 0 {
		t.Fatal("no unified derivative work was performed")
	}
	// Replacing one input retains the old release until that output finishes.
	hold = m.wire.pause(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/future/") && strings.HasPrefix(r.Header.Get("Content-Type"), "image/webp")
	})
	before, err := m.app.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpEdit, Path: "images/one.png", Edit: &media.Edit{Rotate: 90}}}})
	must(t, err)
	select {
	case <-hold.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("replacement image did not reach held output")
	}
	during, err := m.app.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	if len(during.Files) != len(before.Files) || len(during.HLS) != len(before.HLS) {
		t.Fatal("pending replacement changed the completed reader snapshot")
	}
	for i, file := range before.Files {
		current := during.Files[i]
		if current.Path != file.Path || current.Type != file.Type || current.Size != file.Size || current.W != file.W || current.H != file.H {
			t.Fatal("pending replacement changed completed reader media facts")
		}
	}
	hold.unblock()
	awaitMediaReady(t, admin, ref)
}
