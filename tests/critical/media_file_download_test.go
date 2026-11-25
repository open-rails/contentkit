package critical_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
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
	// A player's download serves its original: released into private/ with
	// the version, served by the gateway under the item's token, named
	// {title}{ext}, never stored twice.
	f := newTinyHLSConfiguredFixture(t, false, nil)
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
	if !strings.HasPrefix(grant.URL, f.storage.gateway.URL+layout.URLPrefix) || !strings.HasSuffix(grant.Name, ".mp4") {
		t.Fatalf("an original download is the gateway's file named {title}{ext}: %+v", grant)
	}
	response, err := http.Get(grant.URL)
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	disposition, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	must(t, err)
	if response.StatusCode != 200 || disposition != "attachment" || params["filename"] != grant.Name || !bytes.Equal(body, f.Source) || int64(len(body)) != grant.Size {
		t.Fatalf("original download: HTTP%d %v length %d grant %+v", response.StatusCode, response.Header, len(body), grant)
	}
	config, err := a.cfg.MediaKinds()
	must(t, err)
	reg, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	ref, err := reg.VersionRef(f.Ref.Kind, f.Ref.ID, f.Ref.Version)
	must(t, err)
	manifest, _, err := mustManifests(t, f.storage.store, reg).Get(t.Context(), ref)
	must(t, err)
	file, found := manifest.Get("source.mp4")
	if !found || layout.BlobArea(file.Blob) != layout.AreaPrivate {
		t.Fatalf("the released original is not in private/: %+v", file)
	}
	for _, other := range manifest.Files {
		if other.Path != file.Path && other.Blob == layout.MoveRef(file.Blob, layout.AreaInternal) {
			t.Fatal("the original is stored twice")
		}
	}
	policy.denied.Store(true)
	_, err = reader.GetContentVersionDownload(t.Context(), f.Ref.ID, f.Ref.Version)
	requireError(t, err, 403, "download_unavailable")
	policy.denied.Store(false)
	// Withdrawal moves the original back to internal/, which no token opens.
	_, err = a.engine.Client().PostContentUnpublish(t.Context(), f.Ref.ID)
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool {
		response, err := http.Get(grant.URL)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 404
	})
}

// A gallery whose page source cannot be read must refuse the download up
// front, before a link is issued or the viewer's daily download is spent.
func TestZIPDownloadRefusesUnreadablePagesBeforeCharging(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Config.Kinds[0]
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
		kind.Private = append(kind.Private,
			media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}})
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
	// Reads trust the item cache's revision: drop its row, as a rebuild
	// would find it, so the next read fetches the rewritten manifest.
	_, err = w.pool.Exec(t.Context(), `DELETE FROM `+pgx.Identifier{w.schema, "content_media_items"}.Sanitize()+` WHERE content_version_id=$1`, ref.Version)
	must(t, err)

	// Twice under a one-download quota: a charged refusal would make the second 429.
	for range 2 {
		_, err = reader.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
		requireError(t, err, http.StatusConflict, "no_download")
	}
}

// Download tickets are signed with the delivery ring's current key and verify
// under its previous one: a ticket from before a rotation still transfers,
// and stops once the previous key is dropped.
func TestDeliveryRotationKeepsTickets(t *testing.T) {
	// A gallery's pages ZIP is a ticketed transfer (a video's original is a
	// gateway URL under the item's token).
	g := newReleasedGallery(t, nil, chapterImage(t, 21))
	g.publish(t)
	g.awaitReleased(t)
	a := g.app
	ref := g.ref
	restart := func(t *testing.T, signing, previous string) {
		t.Helper()
		a.server.Close()
		must(t, a.engine.Close(t.Context()))
		a.cfg.Media.Delivery.SigningKey, a.cfg.Media.Delivery.PreviousKey = signing, previous
		a.deps.Downloads = &transferDownloadPolicy{}
		var err error
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		must(t, a.engine.StartJobs(t.Context()))
	}
	ticket := func(t *testing.T) string {
		t.Helper()
		var grant versions.FileDownload
		waitForWithin(t, 45*time.Second, func() bool {
			reply, err := a.client("").GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
			must(t, err)
			if reply.Status == http.StatusAccepted {
				return false
			}
			must(t, reply.Decode(&grant))
			return grant.URL != ""
		})
		if !strings.HasPrefix(grant.URL, "archive?") {
			t.Fatalf("download is not a ticketed transfer: %s", grant.URL)
		}
		return grant.URL
	}
	transfer := func(t *testing.T, ticket string) int {
		t.Helper()
		response, err := http.Get(a.server.URL + "/content/v1/contents/" + ref.ID + "/versions/" + ref.Version + "/" + ticket)
		must(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		must(t, err)
		return response.StatusCode
	}
	original := g.storage.signing
	restart(t, original, "")
	old := ticket(t)
	if code := transfer(t, old); code != http.StatusOK {
		t.Fatalf("fresh ticket transferred HTTP%d", code)
	}
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	must(t, err)
	rotated := "rotated:" + base64.StdEncoding.EncodeToString(secret)
	restart(t, rotated, original)
	if code := transfer(t, old); code != http.StatusOK {
		t.Fatalf("ticket signed before the rotation transferred HTTP%d under its previous key", code)
	}
	fresh := ticket(t)
	if code := transfer(t, fresh); code != http.StatusOK {
		t.Fatalf("ticket after the rotation transferred HTTP%d", code)
	}
	restart(t, rotated, "")
	if code := transfer(t, old); code != http.StatusNotFound {
		t.Fatalf("ticket of a dropped key transferred HTTP%d", code)
	}
	if code := transfer(t, fresh); code != http.StatusOK {
		t.Fatalf("current-key ticket transferred HTTP%d", code)
	}
}
