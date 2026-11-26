package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
)

// invertHook is a master hook that changes every pixel of a PNG.
type invertHook struct{ calls atomic.Int64 }

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

// failingHook is a master hook whose job always fails.
type failingHook struct{ calls atomic.Int64 }

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
	jobs, err := river.NewClient(riverpgxv5.New(g.world.pool), &river.Config{Schema: g.app.cfg.Media.WorkerSchema})
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
			wc.Masters = map[string]media.MasterHook{"invert": hook}
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
		before := g.awaitReleased(t)
		settleDerivatives(t, g)
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
			wc.Masters = map[string]media.MasterHook{"broken": hook}
		})
		_, err := g.app.engine.RegenerateMedia(t.Context(), media.Regeneration{Kind: "gallery"})
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
