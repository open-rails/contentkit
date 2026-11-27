package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/worker"
)

// invertHook is a master hook that changes every pixel of a PNG.
type invertHook struct{ calls atomic.Int64 }

func (*invertHook) Name() string    { return "invert" }
func (*invertHook) Version() string { return "1" }

func (h *invertHook) Process(_ context.Context, in media.MasterInput) (media.MasterOutput, error) {
	h.calls.Add(1)
	f, err := os.Open(in.Path)
	if err != nil {
		return media.MasterOutput{}, err
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		return media.MasterOutput{}, err
	}
	b := src.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			out.SetNRGBA(x, y, color.NRGBA{R: 255 - c.R, G: 255 - c.G, B: 255 - c.B, A: c.A})
		}
	}
	w, err := os.Create(in.Out)
	if err != nil {
		return media.MasterOutput{}, err
	}
	if err := png.Encode(w, out); err != nil {
		w.Close()
		return media.MasterOutput{}, err
	}
	return media.MasterOutput{Type: media.MasterPNG}, w.Close()
}

// negateHook is a video master hook: every pixel negated, written as FFV1 in
// Matroska with the source's other tracks, as a restoration hook would.
type negateHook struct{ calls atomic.Int64 }

func (*negateHook) Name() string    { return "negate" }
func (*negateHook) Version() string { return "1" }

func (h *negateHook) Process(ctx context.Context, in media.MasterInput) (media.MasterOutput, error) {
	h.calls.Add(1)
	if !strings.HasPrefix(in.Type, "video/") {
		return media.MasterOutput{Unchanged: true}, nil
	}
	out, err := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", in.Path, "-map", "0",
		"-vf", "negate", "-c:v", "ffv1", "-c:a", "copy", "-f", "matroska", in.Out).CombinedOutput()
	if err != nil {
		return media.MasterOutput{}, fmt.Errorf("negate %s: %v: %s", in.File, err, out)
	}
	return media.MasterOutput{Type: media.MasterMatroska}, nil
}

// fetchPNG decodes the PNG at url.
func fetchPNG(t *testing.T, url string) image.Image {
	t.Helper()
	res, err := http.Get(url)
	must(t, err)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP%d", url, res.StatusCode)
	}
	img, err := png.Decode(res.Body)
	must(t, err)
	return img
}

// negative is img with every pixel negated.
func negative(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			out.SetNRGBA(x, y, color.NRGBA{R: 255 - c.R, G: 255 - c.G, B: 255 - c.B, A: c.A})
		}
	}
	return out
}

// workerBinary is the worker command .bin holds, built before the suite.
func workerBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(".bin", name))
	must(t, err)
	if info, err := os.Stat(path); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("build %s before this selected test (.github/scripts/build-and-vet.sh)", path)
	}
	return path
}

// workerEnv is a worker process's environment for m's database and bucket.
func workerEnv(t *testing.T, m *mediaWorld, kindsFile string) []string {
	c := m.workerConfig
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + os.Getenv("CONTENTKIT_CRITICAL_PG"), "MEDIA_KINDS_FILE=" + kindsFile,
		"MEDIA_METRICS_ADDR=127.0.0.1:0", "MEDIA_WORKER_SCHEMA=" + c.Schema, "MEDIA_CONTENT_SCHEMA=" + c.ContentSchema,
		"MEDIA_HOST_RIVER_SCHEMA=" + c.HostSchema, "MEDIA_WORKER_TMP=" + t.TempDir(), "MEDIA_WORKER_THREADS=1", "MEDIA_WORKER_SHUTDOWN_GRACE=1s",
		"MEDIA_S3_BUCKET=" + m.bucket, "MEDIA_S3_ENDPOINT=" + os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"),
		"MEDIA_S3_ACCESS_KEY_ID=" + os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), "MEDIA_S3_SECRET_ACCESS_KEY=" + os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD")}
	if c.AccountSchema != "" {
		env = append(env, "MEDIA_ACCOUNT_SCHEMA="+c.AccountSchema, "MEDIA_ACCOUNT_WORKER_SCHEMA="+c.AccountWorkerSchema)
	}
	return env
}

// brokenReader fails every read past at, as a disk error would.
type brokenReader struct {
	data []byte
	at   int64
}

func (r brokenReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > r.at {
		return 0, errors.New("injected: the file could not be read")
	}
	return copy(p, r.data[off:]), nil
}

// failingHook is a master hook whose job always fails.
type failingHook struct{ calls atomic.Int64 }

func (*failingHook) Name() string    { return "broken" }
func (*failingHook) Version() string { return "1" }

func (h *failingHook) Process(context.Context, media.MasterInput) (media.MasterOutput, error) {
	h.calls.Add(1)
	return media.MasterOutput{}, errors.New("injected: the master hook's job failed")
}

func blobHashOf(ref string) string {
	_, h, _ := layout.SplitBlobRef(ref)
	return h
}

// pagePaths are a released gallery's page uploads.
func pagePaths(m *mediastore.Manifest) []string {
	var out []string
	for _, f := range m.Files {
		if f.IsUpload() && strings.HasPrefix(f.Path, "pages/") {
			out = append(out, f.Path)
		}
	}
	return out
}

// itemObjects lists the item folder's stored files (not its manifest) by key.
func itemObjects(t *testing.T, g *releasedGallery) map[string]bool {
	t.Helper()
	item, err := g.reg.Item(g.cref)
	must(t, err)
	out := map[string]bool{}
	for obj, err := range g.storage.store.List(t.Context(), item.Prefix()) {
		must(t, err)
		if !strings.HasSuffix(obj.Key, layout.ManifestName) {
			out[strings.TrimPrefix(obj.Key, item.Prefix())] = true
		}
	}
	return out
}

// settleDerivatives waits until no media derivative job is queued or running.
func settleDerivatives(t *testing.T, g *releasedGallery) {
	t.Helper()
	settleMedia(t, g.world.pool, g.app.cfg.Media.WorkerSchema)
}

// settleMedia waits until no media job of the worker schema is queued or
// running.
func settleMedia(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	must(t, err)
	waitForWithin(t, 60*time.Second, func() bool {
		pending, err := jobs.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.IngestArgs{}).Kind(), (workqueue.DerivativeArgs{}).Kind()).States(
			rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStatePending).First(1))
		must(t, err)
		return len(pending.Jobs) == 0
	})
}

func (g *releasedGallery) released(t *testing.T) bool {
	t.Helper()
	var released bool
	must(t, g.world.pool.QueryRow(t.Context(), `SELECT released FROM `+pgx.Identifier{g.world.schema, "content_versions"}.Sanitize()+` WHERE id=$1`, g.ref.Version).Scan(&released))
	return released
}

// downloadZip fetches the version's ZIP as client and returns its entries.
func (g *releasedGallery) downloadZip(t *testing.T, client *ck.Client) map[string][]byte {
	t.Helper()
	res, body := g.transfer(t, client, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ZIP download HTTP%d", res.StatusCode)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	must(t, err)
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		must(t, err)
		b, err := io.ReadAll(rc)
		rc.Close()
		must(t, err)
		out[f.Name] = b
	}
	return out
}

// transfer grants client a download and follows it, with rangeHeader.
func (g *releasedGallery) transfer(t *testing.T, client *ck.Client, rangeHeader string) (*http.Response, []byte) {
	t.Helper()
	var grant versions.FileDownload
	waitForWithin(t, 45*time.Second, func() bool {
		reply, err := client.GetContentVersionDownload(t.Context(), g.ref.ID, g.ref.Version)
		must(t, err)
		return reply.Status != http.StatusAccepted && json.Unmarshal(reply.Body, &grant) == nil && grant.URL != ""
	})
	base, err := url.Parse(g.app.server.URL + "/content/v1/contents/" + g.ref.ID + "/versions/" + g.ref.Version + "/download")
	must(t, err)
	target, err := base.Parse(grant.URL)
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.String(), nil)
	must(t, err)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	must(t, err)
	return res, body
}

// TestMediaMasters: masters sit between originals and renditions. The
// identity recipe stores nothing for them; a recipe's hooks make masters of
// their own, renditions derive from them, downloads serve them, and a
// failing hook changes nothing.
func TestMediaMasters(t *testing.T) {
	t.Run("CK-J09-S47_the_identity_recipe_stores_no_master_bytes", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 51), chapterImage(t, 53))
		g.publish(t)
		m := g.awaitReleased(t)
		originals := map[string]bool{}
		for _, p := range pagePaths(m) {
			f, _ := m.Get(p)
			if f.Master == nil || f.Master.Blob != f.Blob || f.Master.Recipe != media.IdentityRecipe || f.Master.Of != "" {
				t.Fatalf("an identity master is not its original: %s %+v", f.Blob, f.Master)
			}
			if layout.BlobArea(f.Blob) != layout.AreaInternal {
				t.Fatalf("a release moved original %s", f.Blob)
			}
			originals[blobHashOf(f.Blob)] = true
			for _, r := range m.Outputs(p, "reader") {
				if r.Input != blobHashOf(f.Blob) {
					t.Fatalf("rendition %s does not derive from its master: input %q", r.Path, r.Input)
				}
			}
		}
		if len(originals) != 2 {
			t.Fatalf("pages: %v", originals)
		}
		named := map[string]bool{}
		for _, b := range m.Blobs() {
			named[b] = true
		}
		for _, r := range m.Retired {
			named[r.Blob] = true
		}
		for key := range itemObjects(t, g) {
			if !named[key] {
				t.Fatalf("the bucket holds %s, which the manifest does not name", key)
			}
			if area, h, _ := layout.SplitBlobRef(key); originals[h] && area != layout.AreaInternal {
				t.Fatalf("an original was copied to %s", key)
			}
		}
		// The download is the originals themselves, read in internal/.
		zipped := g.downloadZip(t, g.app.engine.Client())
		if len(zipped) != 2 || !bytes.Equal(zipped["001.png"], chapterImage(t, 51)) || !bytes.Equal(zipped["002.png"], chapterImage(t, 53)) {
			t.Fatalf("ZIP entries are not the originals: %d", len(zipped))
		}
	})

	t.Run("CK-J09-S48_a_changed_rendition_spec_regenerates_exactly_the_selected_items", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 61))
		g.publish(t)
		g.awaitReleased(t)
		other := *g
		admin := g.app.engine.Client()
		other.ref = galleryRef(t, admin, "Untouched by a selected regeneration")
		_, err := admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: other.ref, OperationID: uuid.NewString(), Ops: []media.Op{stageFile(t, admin, other.ref, "pages/001.png", chapterImage(t, 67))}})
		must(t, err)
		awaitMediaReady(t, admin, other.ref)
		other.cref, err = g.reg.VersionRef("gallery", other.ref.ID, other.ref.Version)
		must(t, err)
		other.publish(t)
		other.awaitReleased(t)
		settleDerivatives(t, g)
		before, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		untouched, _, err := g.manifests.Get(t.Context(), other.cref)
		must(t, err)
		old := before.Outputs("pages/001.png", "reader")[0]
		// A deploy changes the reader rendition's spec.
		g.storage.restart(t, g.app, func(c *ck.Config, _ *worker.Config) {
			c.Media.Kinds[0].Private[0].Image = &media.Image{Width: 40, Height: 40}
		})
		n, err := g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery", IDs: []string{g.ref.ID}})
		must(t, err)
		if n != 1 {
			t.Fatalf("a regeneration of one item queued %d", n)
		}
		var after *mediastore.Manifest
		waitForWithin(t, 60*time.Second, func() bool {
			cur, _, err := g.manifests.Get(t.Context(), g.cref)
			must(t, err)
			outs := cur.Outputs("pages/001.png", "reader")
			after = cur
			return len(outs) == 1 && outs[0].FP != old.FP && layout.Served(layout.BlobArea(outs[0].Blob))
		})
		settleDerivatives(t, g)
		now := after.Outputs("pages/001.png", "reader")[0]
		if now.Blob == old.Blob || now.W > 40 || now.Input != old.Input {
			t.Fatalf("the regenerated rendition: %+v from %+v", now, old)
		}
		if r, ok := after.Retiring(old.Blob); !ok || r.Due.Sub(r.At) < 24*time.Hour {
			t.Fatalf("the replaced rendition is not retired: %+v", r)
		}
		page, _ := after.Get("pages/001.png")
		if page.Master == nil || page.Master.Blob != page.Blob {
			t.Fatalf("a spec change touched the master: %+v", page.Master)
		}
		if !g.released(t) {
			t.Fatal("the regenerated version is no longer released")
		}
		cur, _, err := g.manifests.Get(t.Context(), other.cref)
		must(t, err)
		if cur.Rev != untouched.Rev {
			t.Fatalf("an unselected item was regenerated: revision %d -> %d", untouched.Rev, cur.Rev)
		}
		// The whole kind: the other item changes, the current one writes nothing.
		rev := after.Rev
		n, err = g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
		must(t, err)
		if n != 2 {
			t.Fatalf("a kind's regeneration queued %d of 2 items", n)
		}
		waitForWithin(t, 60*time.Second, func() bool {
			cur, _, err := g.manifests.Get(t.Context(), other.cref)
			must(t, err)
			outs := cur.Outputs("pages/001.png", "reader")
			return len(outs) == 1 && outs[0].FP != untouched.Outputs("pages/001.png", "reader")[0].FP && layout.Served(layout.BlobArea(outs[0].Blob))
		})
		settleDerivatives(t, g)
		again, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		if again.Rev != rev {
			t.Fatalf("a current item was regenerated again: revision %d -> %d", rev, again.Rev)
		}
	})

	t.Run("CK-J09-S49_a_recipe_change_rederives_masters_then_renditions", func(t *testing.T) {
		pages := [][]byte{chapterImage(t, 71), chapterImage(t, 73)}
		g := newReleasedGallery(t, nil, pages...)
		g.publish(t)
		before := g.awaitReleased(t)
		settleDerivatives(t, g)
		hook := &invertHook{}
		recipe := &media.Recipe{Steps: []media.RecipeStep{{Hook: "invert", Version: "1", Params: map[string]any{"mode": "rgb"}}}}
		g.storage.restart(t, g.app, func(c *ck.Config, wc *worker.Config) {
			c.Media.Kinds[0].Master = recipe
			wc.Hooks = []media.MasterHook{hook}
		})
		_, err := g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
		must(t, err)
		var after *mediastore.Manifest
		waitForWithin(t, 90*time.Second, func() bool {
			cur, _, err := g.manifests.Get(t.Context(), g.cref)
			must(t, err)
			for _, p := range pagePaths(cur) {
				f, _ := cur.Get(p)
				outs := cur.Outputs(p, "reader")
				if f.Master == nil || f.Master.Of == "" || len(outs) != 1 || outs[0].Input != blobHashOf(f.Master.Blob) ||
					layout.BlobArea(f.Master.Blob) != layout.AreaPrivate || !layout.Served(layout.BlobArea(outs[0].Blob)) {
					return false
				}
			}
			after = cur
			return true
		})
		settleDerivatives(t, g)
		made := int64(0)
		for _, f := range after.Files {
			if f.Master != nil && f.Master.Of != "" {
				made++
			}
		}
		if made < int64(len(pages)) || hook.calls.Load() != made {
			t.Fatalf("the hook ran %d times for %d masters of %d pages", hook.calls.Load(), made, len(pages))
		}
		item, err := g.reg.Item(g.cref)
		must(t, err)
		for i, p := range pagePaths(after) {
			f, _ := after.Get(p)
			was, _ := before.Get(p)
			if f.Blob != was.Blob || f.Master.Of != blobHashOf(f.Blob) || f.Master.Recipe != recipe.FP() || len(f.Master.Steps) != 1 ||
				f.Master.Steps[0].Hook != "invert" || f.Master.Steps[0].Version != "1" || f.Master.Type != media.MasterWebP || f.Master.W != 128 || f.Master.H != 96 {
				t.Fatalf("master of %s: %+v (original %s)", p, f.Master, f.Blob)
			}
			key, err := item.Blob(f.Master.Blob)
			must(t, err)
			rc, _, err := g.storage.store.Get(t.Context(), key, media.GetOptions{})
			must(t, err)
			stored, err := io.ReadAll(rc)
			rc.Close()
			must(t, err)
			if len(stored) < 16 || string(stored[:4]) != "RIFF" || string(stored[8:16]) != "WEBPVP8L" || bytes.Equal(stored, pages[i]) {
				t.Fatalf("master of %s is not a lossless WebP of its own", p)
			}
			r := after.Outputs(p, "reader")[0]
			old := before.Outputs(p, "reader")[0]
			if r.Blob == old.Blob || r.FP == old.FP {
				t.Fatalf("rendition of %s was not re-derived from its new master", p)
			}
			if _, ok := after.Retiring(old.Blob); !ok {
				t.Fatalf("the rendition replaced for %s is not retired", p)
			}
		}
		if !g.released(t) {
			t.Fatal("the version is no longer released")
		}
		// Downloads serve the masters.
		zipped := g.downloadZip(t, g.app.engine.Client())
		for _, p := range pagePaths(after) {
			f, _ := after.Get(p)
			key, err := item.Blob(f.Master.Blob)
			must(t, err)
			rc, _, err := g.storage.store.Get(t.Context(), key, media.GetOptions{})
			must(t, err)
			stored, err := io.ReadAll(rc)
			rc.Close()
			must(t, err)
			name := strings.TrimSuffix(strings.TrimPrefix(p, "pages/"), ".png") + ".webp"
			if !bytes.Equal(zipped[name], stored) {
				t.Fatalf("ZIP entry %s is not the master (entries %d)", name, len(zipped))
			}
		}
		// The same recipe again: nothing is made or written.
		rev := after.Rev
		_, err = g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
		must(t, err)
		settleDerivatives(t, g)
		again, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		if again.Rev != rev || hook.calls.Load() != made {
			t.Fatalf("a current master was made again: revision %d -> %d, %d hook runs", rev, again.Rev, hook.calls.Load())
		}
	})

	t.Run("CK-J09-S50_a_failed_hook_leaves_the_release_unchanged", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 81))
		g.publish(t)
		g.awaitReleased(t)
		// Readers are served before processing and the release write their
		// last: the baseline is the manifest once both have settled.
		settleDerivatives(t, g)
		awaitDuePublications(t, g.app)
		before, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		objects := itemObjects(t, g)
		reader := g.app.client("")
		var read media.ReadResult
		waitForWithin(t, 45*time.Second, func() bool {
			var err error
			read, err = reader.GetMediaVersion(t.Context(), g.ref.Kind, g.ref.ID, g.ref.Version, ck.GetMediaVersionQuery{})
			return err == nil && len(read.Files) > 0 && g.released(t)
		})
		hook := &failingHook{}
		g.storage.restart(t, g.app, func(c *ck.Config, wc *worker.Config) {
			c.Media.Kinds[0].Master = &media.Recipe{Steps: []media.RecipeStep{{Hook: "broken", Version: "1"}}}
			wc.Hooks = []media.MasterHook{hook}
		})
		_, err = g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
		must(t, err)
		jobs, err := river.NewClient(riverpgxv5.New(g.world.pool), &river.Config{Schema: g.app.cfg.Media.WorkerSchema})
		must(t, err)
		waitForWithin(t, 60*time.Second, func() bool {
			list, err := jobs.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()).States(rivertype.JobStateRetryable))
			must(t, err)
			for _, j := range list.Jobs {
				if len(j.Errors) > 0 && strings.Contains(j.Errors[len(j.Errors)-1].Error, "injected: the master hook") {
					return true
				}
			}
			return false
		})
		if hook.calls.Load() == 0 {
			t.Fatal("the failing hook never ran")
		}
		after, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		if after.Rev != before.Rev {
			t.Fatalf("a failed hook rewrote the manifest: revision %d -> %d", before.Rev, after.Rev)
		}
		if got := itemObjects(t, g); len(got) != len(objects) {
			t.Fatalf("a failed hook stored files: %d -> %d", len(objects), len(got))
		}
		if !g.released(t) {
			t.Fatal("a failed hook withdrew the release")
		}
		now, err := g.app.client("").GetMediaVersion(t.Context(), g.ref.Kind, g.ref.ID, g.ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		if len(now.Files) != len(read.Files) || len(now.Files) == 0 || now.Files[0].Path != read.Files[0].Path {
			t.Fatalf("a failed hook changed what readers get: %d -> %d files", len(read.Files), len(now.Files))
		}
	})

	t.Run("CK-J09-S52_a_video_master_rederives_its_renditions_sprite_frames_and_thumbnails", func(t *testing.T) {
		f := newTinyHLSConfiguredFixture(t, false, withPosters)
		admin := f.app.engine.Client()
		at := 1.0
		_, err := admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: f.Ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpFrame, Path: "poster", T: &at}}})
		must(t, err)
		oldPoster := fetchPNG(t, awaitPoster(t, admin, f.Ref).URL)
		settleMedia(t, f.world.pool, f.app.cfg.Media.WorkerSchema)
		manifests, cref := failureManifests(t, f.resilientMedia, f.Ref)
		before, _, err := manifests.Get(t.Context(), cref)
		must(t, err)
		hook := &negateHook{}
		f.restart(t, func(c *ck.Config, wc *worker.Config) {
			for i := range c.Media.Kinds {
				if c.Media.Kinds[i].Name == "clip" {
					c.Media.Kinds[i].Master = &media.Recipe{Steps: []media.RecipeStep{{Hook: "negate", Version: "1"}}}
				}
			}
			wc.Hooks = []media.MasterHook{hook}
		})
		n, err := f.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "clip", IDs: []string{f.Ref.ID}})
		must(t, err)
		if n != 1 {
			t.Fatalf("a regeneration of one clip queued %d", n)
		}
		// Everything derived from the video follows its new master: the HLS
		// renditions with their sprite, the grabbed poster and its thumbnail.
		waitForWithin(t, 120*time.Second, func() bool {
			cur, _, err := manifests.Get(t.Context(), cref)
			must(t, err)
			v, _ := cur.Get("source.mp4")
			if v.Master == nil || v.Master.Of == "" || layout.BlobArea(v.Master.Blob) != layout.AreaPrivate {
				return false
			}
			made := blobHashOf(v.Master.Blob)
			outs := cur.Outputs("source.mp4", "stream")
			if len(outs) == 0 {
				return false
			}
			for _, o := range outs {
				if o.Input != made || !layout.Served(layout.BlobArea(o.Blob)) {
					return false
				}
			}
			p, _ := cur.Get("poster.png")
			thumbs := cur.Outputs("poster.png", "poster")
			if p.Frame == nil || blobHashOf(p.Frame.Of) != made || len(thumbs) != 1 || thumbs[0].Input != blobHashOf(p.Blob) ||
				!layout.Served(layout.BlobArea(thumbs[0].Blob)) {
				return false
			}
			return true
		})
		// Released: what the release moves is served, so nothing more is written.
		settleMedia(t, f.world.pool, f.app.cfg.Media.WorkerSchema)
		after, _, err := manifests.Get(t.Context(), cref)
		must(t, err)
		video, _ := after.Get("source.mp4")
		was, _ := before.Get("source.mp4")
		if video.Blob != was.Blob || video.Master.Of != blobHashOf(video.Blob) || video.Master.Type != media.MasterMatroska || hook.calls.Load() != 1 {
			t.Fatalf("video master %+v of original %s; the hook ran %d times", video.Master, video.Blob, hook.calls.Load())
		}
		item, err := f.registry.Item(cref)
		must(t, err)
		key, err := item.Blob(video.Master.Blob)
		must(t, err)
		rc, _, err := f.storage.store.Get(t.Context(), key, media.GetOptions{})
		must(t, err)
		stored, err := io.ReadAll(rc)
		rc.Close()
		must(t, err)
		if !bytes.HasPrefix(stored, []byte{0x1a, 0x45, 0xdf, 0xa3}) || !bytes.Contains(stored, []byte("FFV1")) {
			t.Fatal("the video master is not FFV1 in Matroska")
		}
		old := map[string]media.File{}
		for _, o := range before.Outputs("source.mp4", "stream") {
			old[o.Path] = o
		}
		sprite := false
		for _, o := range after.Outputs("source.mp4", "stream") {
			if prev, ok := old[o.Path]; ok && (prev.Blob == o.Blob || prev.FP == o.FP) {
				t.Fatalf("%s was not re-derived from the new master", o.Path)
			}
			sprite = sprite || o.Track != nil && o.Track.Kind == media.TrackSprite
		}
		if !sprite {
			t.Fatal("the new master has no sprite")
		}
		poster, _ := after.Get("poster.png")
		oldFrame, _ := before.Get("poster.png")
		thumb, oldThumb := after.Outputs("poster.png", "poster")[0], before.Outputs("poster.png", "poster")[0]
		if poster.Blob == oldFrame.Blob || poster.Frame.T != oldFrame.Frame.T || thumb.Blob == oldThumb.Blob {
			t.Fatalf("the poster %s and its thumbnail %s were not made again", poster.Blob, thumb.Blob)
		}
		var newPoster media.FileInfo
		for _, file := range editorRead(t, admin, f.Ref).Files {
			if file.Upload && file.Path == "poster.png" {
				newPoster = file
			}
		}
		grabbed := fetchPNG(t, newPoster.URL)
		if near, far := frameDistance(grabbed, negative(oldPoster)), frameDistance(grabbed, oldPoster); near > 8 || far < 4*near+20 {
			t.Fatalf("the poster is not grabbed from the negated master: %.1f from the negative, %.1f from the old poster", near, far)
		}
		// Readers stream the new renditions.
		master, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{})
		must(t, err)
		child, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, path.Join(path.Dir(f.MasterPath), playlistURIForTest(t, master.Body)), ck.GetMediaVersionHlsQuery{})
		must(t, err)
		served := false
		for _, o := range after.Outputs("source.mp4", "stream") {
			served = served || o.Track != nil && o.Track.Kind == media.TrackVideo && bytes.Contains(child.Body, []byte(blobHashOf(o.Blob)))
		}
		if master.Status != http.StatusOK || !served {
			t.Fatalf("readers do not stream the new renditions: HTTP%d %s", master.Status, child.Body)
		}
		// The same recipe again: nothing is made or written. The editor read
		// above queued the new poster's editor view: settle it first.
		settleMedia(t, f.world.pool, f.app.cfg.Media.WorkerSchema)
		settled, _, err := manifests.Get(t.Context(), cref)
		must(t, err)
		_, err = f.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "clip", IDs: []string{f.Ref.ID}})
		must(t, err)
		settleMedia(t, f.world.pool, f.app.cfg.Media.WorkerSchema)
		again, _, err := manifests.Get(t.Context(), cref)
		must(t, err)
		if again.Rev != settled.Rev || hook.calls.Load() != 1 {
			t.Fatalf("a current video master was made again: revision %d -> %d, %d hook runs", settled.Rev, again.Rev, hook.calls.Load())
		}
	})

	t.Run("CK-J09-S53_a_hosts_worker_binary_registers_its_hooks_and_one_without_them_never_starts", func(t *testing.T) {
		stock, host := workerBinary(t, "media-worker"), workerBinary(t, "host-media-worker")
		page := chapterImage(t, 101)
		g := newReleasedGallery(t, nil, page)
		g.publish(t)
		g.awaitReleased(t)
		settleDerivatives(t, g)
		g.storage.handOff(t, g.app, func(c *ck.Config) {
			c.Media.Kinds[0].Master = &media.Recipe{Steps: []media.RecipeStep{{Hook: "invert", Version: "1"}}}
		})
		kinds, err := g.app.cfg.MediaKinds()
		must(t, err)
		// The public entry point refuses a recipe's missing or other hook
		// version, and two hooks of one name, before it touches a job.
		config := g.storage.workerConfig
		config.Kinds = kinds
		other := kinds
		other.Kinds = slices.Clone(kinds.Kinds)
		for i := range other.Kinds {
			if other.Kinds[i].Master != nil {
				other.Kinds[i].Master = &media.Recipe{Steps: []media.RecipeStep{{Hook: "invert", Version: "2"}}}
			}
		}
		for _, c := range []struct {
			kinds media.Config
			hooks []media.MasterHook
			want  string
		}{
			{kinds, nil, `master hook "invert" is not registered`},
			{other, []media.MasterHook{&invertHook{}}, `master hook "invert" is version "1", its recipe names "2"`},
			{kinds, []media.MasterHook{&invertHook{}, &invertHook{}}, `two master hooks are named "invert"`},
		} {
			config.Kinds, config.Hooks = c.kinds, c.hooks
			if _, err := worker.New(t.Context(), config); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("worker.New: %v, want %q", err, c.want)
			}
		}
		_, err = g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
		must(t, err)
		registry, err := g.app.cfg.MediaRegistry()
		must(t, err)
		kindsFile := filepath.Join(t.TempDir(), "kinds.json")
		must(t, os.WriteFile(kindsFile, registry, 0o600))
		env := workerEnv(t, g.storage, kindsFile)
		before, _, err := g.manifests.Get(t.Context(), g.cref)
		must(t, err)
		// The stock image has no hooks: it exits at start and takes no job.
		refused := exec.CommandContext(t.Context(), stock)
		refused.Env = env
		var logs bytes.Buffer
		refused.Stdout, refused.Stderr = &logs, &logs
		if err := refused.Run(); err == nil || !strings.Contains(logs.String(), `master hook \"invert\" is not registered`) {
			t.Fatalf("the stock worker started without the recipe's hook: %v\n%s", err, logs.String())
		}
		if cur, _, err := g.manifests.Get(t.Context(), g.cref); err != nil || cur.Rev != before.Rev {
			t.Fatalf("a refused worker wrote the manifest: %v", err)
		}
		// The host's own binary, worker.Main with its hook, makes the masters.
		run := exec.Command(host)
		run.Env = env
		var hostLogs bytes.Buffer
		run.Stdout, run.Stderr = &hostLogs, &hostLogs
		must(t, run.Start())
		var exitErr error
		exited := make(chan struct{})
		go func() { exitErr = run.Wait(); close(exited) }()
		t.Cleanup(func() {
			_ = run.Process.Kill()
			<-exited
		})
		var after *mediastore.Manifest
		waitForWithin(t, 90*time.Second, func() bool {
			select {
			case <-exited:
				t.Fatalf("the host worker exited: %v\n%s", exitErr, hostLogs.String())
			default:
			}
			cur, _, err := g.manifests.Get(t.Context(), g.cref)
			must(t, err)
			f, _ := cur.Get("pages/001.png")
			outs := cur.Outputs(f.Path, "reader")
			if f.Master == nil || f.Master.Of == "" || layout.BlobArea(f.Master.Blob) != layout.AreaPrivate || len(outs) != 1 || outs[0].Input != blobHashOf(f.Master.Blob) {
				return false
			}
			after = cur
			return true
		})
		f, _ := after.Get("pages/001.png")
		if len(f.Master.Steps) != 1 || f.Master.Steps[0].Hook != "invert" || f.Master.Type != media.MasterWebP {
			t.Fatalf("host master: %+v", f.Master)
		}
		item, err := g.reg.Item(g.cref)
		must(t, err)
		key, err := item.Blob(f.Master.Blob)
		must(t, err)
		rc, _, err := g.storage.store.Get(t.Context(), key, media.GetOptions{})
		must(t, err)
		made, _, err := image.Decode(rc)
		rc.Close()
		must(t, err)
		original, err := png.Decode(bytes.NewReader(page))
		must(t, err)
		if d := frameDistance(made, negative(original)); d > 0.5 {
			t.Fatalf("the master is not the host hook's lossless result: distance %.2f", d)
		}
		must(t, run.Process.Signal(syscall.SIGTERM))
		select {
		case <-exited:
			if exitErr != nil {
				t.Fatalf("the host worker did not drain: %v\n%s", exitErr, hostLogs.String())
			}
		case <-time.After(45 * time.Second):
			t.Fatal("the host worker did not stop")
		}
	})

	t.Run("CK-J09-S54_a_master_past_the_single_put_limit_goes_up_in_parts", func(t *testing.T) {
		// The writer of hook-made masters and video renditions; parts of S3's
		// smallest size stand in for the 1 GiB limit and 64 MiB parts.
		m := newMediaStorage(t, ck.Scope{TenantID: ck.NewTenantID(), AppID: ck.NewAppID()})
		const part = 5 << 20
		body := make([]byte, 2*part+12345)
		for i := range body {
			body[i] = byte(i*7 + i>>13)
		}
		sum := sha256.Sum256(body)
		opts := mediastore.FileOptions{PutOptions: media.PutOptions{ContentType: media.MasterMatroska, ChecksumSHA256: sum[:]}, SinglePutMax: part, PartSize: part}
		key := "critical/masters/" + uuid.NewString()
		must(t, mediastore.PutFile(t.Context(), m.store, key, bytes.NewReader(body), int64(len(body)), opts))
		rc, obj, err := m.store.Get(t.Context(), key, media.GetOptions{})
		must(t, err)
		stored, err := io.ReadAll(rc)
		rc.Close()
		must(t, err)
		if !bytes.Equal(stored, body) || obj.ContentType != media.MasterMatroska || !strings.HasSuffix(strings.Trim(obj.ETag, `"`), "-3") {
			t.Fatalf("stored %d of %d bytes as %s, ETag %s: not three parts", len(stored), len(body), obj.ContentType, obj.ETag)
		}
		// A file that fails part way leaves no upload behind.
		broken := "critical/masters/" + uuid.NewString()
		err = mediastore.PutFile(t.Context(), m.store, broken, brokenReader{data: body, at: part + 10}, int64(len(body)), opts)
		if err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("a failed read uploaded: %v", err)
		}
		uploads, err := m.management.ListMultipartUploads(t.Context(), &awss3.ListMultipartUploadsInput{Bucket: &m.bucket, Prefix: &broken})
		must(t, err)
		if len(uploads.Uploads) != 0 {
			t.Fatalf("a failed upload was left incomplete: %d", len(uploads.Uploads))
		}
		if _, err := m.store.Head(t.Context(), broken); !errors.Is(err, mediastore.ErrNotFound) {
			t.Fatalf("a failed upload stored an object: %v", err)
		}
	})

	t.Run("CK-J09-S51_a_watermarked_zip_is_never_cached_and_differs_per_download", func(t *testing.T) {
		g := newReleasedGallery(t, nil, chapterImage(t, 91), chapterImage(t, 93))
		g.publish(t)
		g.awaitReleased(t)
		policy := &markingPolicy{}
		g.app.deps.Downloads = policy
		g.storage.restart(t, g.app, func(*ck.Config, *worker.Config) {})
		_, token := g.world.person(t, 0)
		member := g.app.client(token)
		first, a := g.transfer(t, member, "")
		second, b := g.transfer(t, member, "")
		resumed, c := g.transfer(t, member, "bytes=10-99")
		for _, res := range []*http.Response{first, second, resumed} {
			if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "private, no-store" || res.Header.Get("CDN-Cache-Control") != "no-store" ||
				res.Header.Get("ETag") != "" || res.Header.Get("Accept-Ranges") != "" || res.ContentLength >= 0 {
				t.Fatalf("a watermarked ZIP is cacheable: HTTP%d %v length %d", res.StatusCode, res.Header, res.ContentLength)
			}
		}
		if bytes.Equal(a, b) || bytes.Equal(b, c) || policy.marks.Load() < 6 {
			t.Fatalf("watermarked downloads were not marked and zipped anew: %d marks", policy.marks.Load())
		}
		zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
		must(t, err)
		if len(zr.File) != 2 {
			t.Fatalf("watermarked ZIP holds %d entries", len(zr.File))
		}
		// Unwatermarked (no account to mark): the same bytes, with a length.
		anonymous := g.app.client("")
		x, xa := g.transfer(t, anonymous, "")
		y, ya := g.transfer(t, anonymous, "")
		if !bytes.Equal(xa, ya) || x.Header.Get("Content-Length") != strconv.Itoa(len(xa)) || x.Header.Get("ETag") == "" ||
			x.Header.Get("ETag") != y.Header.Get("ETag") || x.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" || bytes.Equal(xa, a) {
			t.Fatalf("an unwatermarked ZIP is not byte-stable: %v", x.Header)
		}
	})
}

// markingPolicy marks every page of a member's download with a mark of its
// own, as a forensic watermark does.
type markingPolicy struct{ marks atomic.Int64 }

func (p *markingPolicy) Decide(_ context.Context, r versions.DownloadRequest) (versions.DownloadDecision, error) {
	d := versions.DownloadDecision{Allowed: true}
	for page := 1; page <= r.Pages; page++ {
		d.Mark = append(d.Mark, page)
	}
	return d, nil
}

func (p *markingPolicy) Mark(_ context.Context, actor access.Actor, _ versions.Target, page int, b []byte) ([]byte, string, error) {
	n := p.marks.Add(1)
	return append(bytes.Clone(b), fmt.Sprintf("WM:%s:%d:%d", actor.ID, page, n)...), ".png", nil
}
