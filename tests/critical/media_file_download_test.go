package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

type transferDownloadPolicy struct{ denied atomic.Bool }

func (p *transferDownloadPolicy) Decide(context.Context, versions.DownloadRequest) (versions.DownloadDecision, error) {
	return versions.DownloadDecision{Allowed: !p.denied.Load(), Reason: "download_disabled"}, nil
}
func (*transferDownloadPolicy) Mark(_ context.Context, _ access.Actor, _ versions.Target, _ int, b []byte) ([]byte, string, error) {
	return b, ".png", nil
}

func TestPublishedSingleFileDownload(t *testing.T) {
	for _, profile := range []string{"internal-original", "host-only-mp4"} {
		t.Run(profile, func(t *testing.T) {
			f := newTinyHLSConfiguredFixture(t, false, func(c *ck.Config) {
				for i := range c.Media.Kinds {
					k := &c.Media.Kinds[i]
					if k.Name != "clip" {
						continue
					}
					if profile == "host-only-mp4" {
						kind := c.Content.Kinds["clip"]
						kind.Download = &kinds.Download{Serve: "export"}
						c.Content.Kinds["clip"] = kind
						k.Private = append(k.Private, media.Private{Name: "export", From: "source", To: "exports/{name}.mp4", HostOnly: true, MP4: media.Rung(144)}, media.Private{Name: "playback", From: "source", To: "playback/{name}.mp4", MP4: media.Rung(144)})
					}
				}
			})
			policy := &transferDownloadPolicy{}
			a := f.app
			a.server.Close()
			must(t, a.engine.Close(t.Context()))
			a.deps.Downloads = policy
			var err error
			a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
			must(t, err)
			a.server = mountEngine(t, a.engine)
			must(t, a.engine.StartJobs(t.Context()))
			reader := a.client("")
			var grant versions.FileDownload
			waitForWithin(t, 45*time.Second, func() bool {
				reply, err := reader.GetContentVersionDownload(t.Context(), f.Ref.ID, f.Ref.Version)
				must(t, err)
				if reply.Status == http.StatusAccepted {
					return false
				}
				must(t, reply.Decode(&grant))
				return grant.URL != ""
			})
			address := grant.URL
			if !strings.HasPrefix(address, "archive?") {
				t.Fatalf("internal download bypassed authorized host transfer: %s", address)
			}
			address = a.server.URL + "/content/v1/contents/" + f.Ref.ID + "/versions/" + f.Ref.Version + "/" + address
			response, err := http.Get(address)
			must(t, err)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			must(t, err)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("download transfer HTTP%d: %s", response.StatusCode, body)
			}
			disposition, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
			must(t, err)
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/octet-stream" || grant.Type != "video/mp4" || disposition != "attachment" || params["filename"] != grant.Name || int64(len(body)) != grant.Size || !bytes.Contains(body, []byte("ftyp")) {
				t.Fatalf("configured download changed type/attachment/bytes: %d %v length%d grant%+v", response.StatusCode, response.Header, len(body), grant)
			}
			if profile != "host-only-mp4" && !bytes.Equal(body, f.Source) {
				t.Fatal("original download changed uploaded bytes")
			}
			if response.Header.Get("Cache-Control") != "private, no-store" {
				t.Fatal("internal authorized transfer was cacheable")
			}
			config, err := a.cfg.MediaKinds()
			must(t, err)
			reg, err := mediastore.NewRegistry(config, mediastore.Hooks{})
			must(t, err)
			ref, err := reg.VersionRef(f.Ref.Kind, f.Ref.ID, f.Ref.Version)
			must(t, err)
			item, err := reg.Item(ref)
			must(t, err)
			journal, err := mediastore.NewPGJournal(f.pool, f.schema, nil)
			must(t, err)
			manifests, err := mediastore.NewManifests(f.storage.store, reg, mediastore.ManifestOptions{Locker: mediastore.PGLocker(f.pool), Journal: journal})
			must(t, err)
			manifest, _, err := manifests.Get(t.Context(), ref)
			must(t, err)
			virtual := "source.mp4"
			expected := layout.AreaOriginals
			if profile == "host-only-mp4" {
				virtual = "exports/source.mp4"
				expected = layout.AreaInternal
			}
			file, found := manifest.Get(virtual)
			if !found {
				t.Fatalf("download source %s absent", virtual)
			}
			area, _, valid := layout.SplitBlobRef(file.Blob)
			if !valid || area != expected {
				t.Fatalf("internal download escaped to %q", file.Blob)
			}
			ring, err := token.NewRing(f.storage.key, nil)
			must(t, err)
			credential := ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), time.Now().Add(time.Minute))
			key, err := item.Blob(file.Blob)
			must(t, err)
			response, err = http.Get(f.storage.gateway.URL + layout.URLPrefix + key + "?t=" + credential)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("whole-item token opened internal original or HostOnly output")
			}
			read, err := reader.GetMediaVersion(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, ck.GetMediaVersionQuery{})
			must(t, err)
			for _, file := range read.Files {
				if file.Path == virtual {
					t.Fatal("ordinary reader lists internal download source")
				}
			}
			changed, err := url.Parse(address)
			must(t, err)
			query := changed.Query()
			query.Set("file", "false")
			changed.RawQuery = query.Encode()
			response, err = http.Get(changed.String())
			must(t, err)
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("single-file ticket was reinterpreted as a ZIP grant")
			}
			policy.denied.Store(true)
			_, err = reader.GetContentVersionDownload(t.Context(), f.Ref.ID, f.Ref.Version)
			requireError(t, err, 403, "download_unavailable")
			response, err = http.Get(address)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != 403 {
				t.Fatal("issued transfer did not recheck the current host download policy")
			}
			policy.denied.Store(false)
			_, err = a.engine.Client().PostContentUnpublish(t.Context(), f.Ref.ID)
			must(t, err)
			waitForWithin(t, 45*time.Second, func() bool {
				response, err := http.Get(address)
				if err != nil {
					return false
				}
				response.Body.Close()
				return response.StatusCode == 404 || response.StatusCode == 403
			})
		})
	}
}

// A gallery whose page source cannot be read must refuse the download up
// front, before a link is issued or the viewer's daily download is spent.
func TestZIPDownloadRefusesUnreadablePagesBeforeCharging(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Config.Kinds[0]
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "archive"}
		kind.Private = append(kind.Private,
			media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}},
			media.Private{Name: "archive", To: "pages.zip", Zip: "download-pages/", HostOnly: true})
		c.Media.Downloads = versions.Downloads{Anonymous: []versions.Quota{{Limit: 1, Per: 24 * time.Hour}}}
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Unreadable gallery")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)

	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	reader := a.client("")
	waitFor(t, func() bool {
		body, _, err := storage.store.Get(t.Context(), item.ManifestKey(), media.GetOptions{})
		must(t, err)
		stored, err := io.ReadAll(body)
		body.Close()
		must(t, err)
		manifest, err := mediastore.DecodeManifest(stored)
		must(t, err)
		page, ok := manifest.Get("download-pages/one.png")
		return ok && page.Blob != ""
	})
	// Point the archive's page at an area the layout does not accept.
	body, _, err := storage.store.Get(t.Context(), item.ManifestKey(), media.GetOptions{})
	must(t, err)
	stored, err := io.ReadAll(body)
	body.Close()
	must(t, err)
	manifest, err := mediastore.DecodeManifest(stored)
	must(t, err)
	for i, f := range manifest.Files {
		if f.Path == "download-pages/one.png" {
			_, name, _ := layout.SplitBlobRef(f.Blob)
			manifest.Files[i].Blob = "future/" + name
		}
	}
	rewritten, err := json.Marshal(manifest)
	must(t, err)
	_, err = storage.store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(rewritten), int64(len(rewritten)), media.PutOptions{ContentType: "application/json"})
	must(t, err)

	// Twice under a one-download quota: a charged refusal would make the second 429.
	for range 2 {
		_, err = reader.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
		requireError(t, err, http.StatusConflict, "no_download")
	}
}
