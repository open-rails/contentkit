package critical_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

func TestOriginalZIPCannotBypassChapterRelease(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Kinds[0]
		kind.Uploads = append(kind.Uploads, media.Upload{Path: "archive", Types: []string{"application/zip"}, ArchiveTo: "pages/{name}", MaxBytes: 1 << 20})
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "released-archive"}
		kind.Private = append(kind.Private, media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}}, media.Private{Name: "released-archive", To: "released.zip", Zip: "download-pages/", HostOnly: true})
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Original archive includes a future chapter")
	first, second := chapterImage(t, 37), chapterImage(t, 89)
	var uploaded bytes.Buffer
	archive := zip.NewWriter(&uploaded)
	for i, body := range [][]byte{first, second} {
		name := []string{"001.png", "020.png"}[i]
		file, err := archive.Create(name)
		must(t, err)
		_, err = file.Write(body)
		must(t, err)
	}
	must(t, archive.Close())
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "archive.zip", Type: "application/zip", Body: bytes.NewReader(uploaded.Bytes())}})
	must(t, err)
	prepared := awaitMediaReady(t, admin, ref)
	var paths []string
	for _, file := range prepared.Files {
		if file.Upload && strings.HasPrefix(file.Path, "pages/") {
			paths = append(paths, file.Path)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("real ZIP did not produce two page originals: %v", paths)
	}
	future := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{{Title: "Released", StartPage: paths[0]}, {Title: "Future", StartPage: paths[1], ReleaseAt: &future}}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	reader := a.client("")
	var grant versions.FileDownload
	waitForWithin(t, 45*time.Second, func() bool {
		reply, err := reader.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
		must(t, err)
		if reply.Status == 202 {
			return false
		}
		must(t, reply.Decode(&grant))
		return grant.URL != ""
	})
	base, err := url.Parse(a.server.URL + "/content/v1/contents/" + ref.ID + "/versions/" + ref.Version + "/download")
	must(t, err)
	relative, err := url.Parse(grant.URL)
	must(t, err)
	response, err := http.Get(base.ResolveReference(relative).String())
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != 200 {
		t.Fatalf("released-set archive failed: HTTP%d %s", response.StatusCode, body)
	}
	released, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	must(t, err)
	if len(released.File) != 1 {
		t.Fatalf("released-set ZIP includes %d files, want one released chapter", len(released.File))
	}
	file, err := released.File[0].Open()
	must(t, err)
	page, err := io.ReadAll(file)
	file.Close()
	must(t, err)
	if !bytes.Equal(page, first) {
		t.Fatal("released archive selected the wrong page bytes")
	}

	// The lower-level media library permits a literal upload as Download.
	// Compose its real catalog/versions interfaces over the same published
	// item, selecting the retained original ZIP rather than the generated ZIP.
	// Do not fabricate an HLS-on-ZIP Engine configuration to reach this seam.
	model, err := a.cfg.MediaKinds()
	must(t, err)
	for i := range model.Kinds {
		if model.Kinds[i].Name == ref.Kind {
			model.Kinds[i].Download = "archive"
			model.Kinds[i].Downloads = append(model.Kinds[i].Downloads, "archive")
		}
	}
	owner := a.cfg.Scope.AppID.String()
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	identity, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = w.pool }))
	cat, err := catalogstore.New(t.Context(), catalogstore.Options{Pool: w.pool, Schema: w.schema, Tenant: owner, Kinds: a.cfg.Content, Auth: identity.Authenticator(), Now: w.clock.now})
	must(t, err)
	service, err := versionstore.New(versionstore.Options{Pool: w.pool, Schema: w.schema, Tenant: owner, Kinds: a.cfg.Content, Auth: identity.Authenticator(), Resolver: cat.Resolver(), Facets: cat, Now: w.clock.now})
	must(t, err)
	registry, err := mediastore.NewRegistry(model, service.Hooks())
	must(t, err)
	queue, err := workqueue.New(w.pool, registry, a.cfg.Media.WorkerSchema)
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, queue)
	must(t, err)
	jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Store: storage.store, Registry: registry, Locker: mediastore.PGLocker(w.pool), Journal: journal})
	must(t, err)
	ring, err := token.NewRing(storage.key, nil)
	must(t, err)
	uploads, err := mediastore.NewUploads(mediastore.UploadOptions{Store: storage.store, Manifests: jobs.Manifests(), Tickets: &ring})
	must(t, err)
	delivery, err := mediastore.NewReader(mediastore.ReaderOptions{Manifests: jobs.Manifests(), Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: storage.key}})
	must(t, err)
	must(t, service.Bind(versionstore.Media{Manifests: jobs.Manifests(), Uploads: uploads, Jobs: jobs, Reader: delivery}))
	raw, err := service.Download(t.Context(), access.Actor{Anonymous: true, IP: "127.0.0.1"}, versions.Target{Content: ref.ID, Version: ref.Version})
	if !errors.Is(err, versionstore.ErrNoDownload) || raw != nil {
		t.Fatalf("raw original ZIP bypassed future chapter release: granted=%t error=%v", raw != nil, err)
	}
}
