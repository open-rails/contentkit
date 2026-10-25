package critical_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
)

// pausePublicCopyResponse lets S3 complete the real public copy, then withholds
// its result from the publisher while a later catalog intent supersedes it.
type pausePublicCopyResponse struct {
	*medias3.Store
	mu             sync.Mutex
	armed          bool
	expectedSource string
	arrived        chan string
	release        chan struct{}
	once           sync.Once
}

func (s *pausePublicCopyResponse) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *pausePublicCopyResponse) Copy(ctx context.Context, source, destination string, options media.CopyOptions) (media.Object, error) {
	object, err := s.Store.Copy(ctx, source, destination, options)
	if err != nil {
		return object, err
	}
	s.mu.Lock()
	pause := s.armed && source == s.expectedSource && strings.Contains(destination, "/public/")
	if pause {
		s.armed = false
	}
	s.mu.Unlock()
	if pause {
		s.arrived <- destination
		select {
		case <-s.release:
		case <-ctx.Done():
			return media.Object{}, ctx.Err()
		}
	}
	return object, nil
}

func TestSupersededPublicCopyReturnsToPrivate(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	paused := &pausePublicCopyResponse{Store: storage.store, arrived: make(chan string, 1), release: make(chan struct{})}
	t.Cleanup(paused.unblock)
	a.deps.Store = paused
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	must(t, a.engine.Start(t.Context()))
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Superseded public storage copy")
	_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.PPV}})
	must(t, err)
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 47))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	requests := pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()
	editions := pgx.Identifier{w.schema, "content_versions"}.Sanitize()
	tenant := a.cfg.Scope.AppID.String()
	var initial string
	waitForWithin(t, 45*time.Second, func() bool {
		err := w.pool.QueryRow(t.Context(), `SELECT coalesce(v.release_id::text,'') FROM `+editions+` v
 JOIN `+requests+` r ON r.tenant_id=v.tenant_id AND r.id=v.release_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.released AND r.state='completed'`, tenant, ref.Version).Scan(&initial)
		return err == nil && initial != ""
	})
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := media.NewRegistry(config)
	must(t, err)
	logical, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(logical)
	must(t, err)
	journal, err := media.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	manifests, err := media.NewManifests(storage.store, registry, media.ManifestOptions{Locker: media.PGLocker(w.pool), Journal: journal})
	must(t, err)
	before, _, err := manifests.Get(t.Context(), logical)
	must(t, err)
	page, found := before.Get("reader/one.webp")
	if !found {
		t.Fatal("initial private release has no reader file")
	}
	area, _, valid := layout.SplitBlobRef(page.Blob)
	if !valid || area != layout.AreaPrivate {
		t.Fatalf("initial placement is %q", page.Blob)
	}
	readerSource, err := item.Blob(page.Blob)
	must(t, err)
	paused.mu.Lock()
	paused.expectedSource = readerSource
	paused.armed = true
	paused.mu.Unlock()
	_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.Public}})
	must(t, err)
	var exposedKey string
	select {
	case exposedKey = <-paused.arrived:
	case <-time.After(45 * time.Second):
		t.Fatal("public publication never reached the actual copy boundary")
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	publicURL := storage.gateway.URL + layout.URLPrefix + exposedKey
	response, err := http.Get(publicURL)
	must(t, err)
	publicBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusOK || len(publicBytes) == 0 {
		t.Fatal("fault did not expose a real copied public object")
	}
	var stale string
	must(t, w.pool.QueryRow(t.Context(), `SELECT id::text FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, ref.Version).Scan(&stale))
	_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.PPV}})
	must(t, err)
	var replacement string
	must(t, w.pool.QueryRow(t.Context(), `SELECT id::text FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, ref.Version).Scan(&replacement))
	if replacement == initial || replacement == stale {
		t.Fatal("private replacement did not supersede the frozen public request")
	}
	paused.unblock()
	waitForWithin(t, 45*time.Second, func() bool {
		var state string
		err := w.pool.QueryRow(t.Context(), `SELECT state FROM `+requests+` WHERE tenant_id=$1 AND id=$2::uuid`, tenant, replacement).Scan(&state)
		return err == nil && state == "completed"
	})
	// Completion is the boundary: do not wait for periodic cleanup or permit
	// a catalog-only no-op to leave B's transient public allocation readable.
	response, err = http.Get(publicURL)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		snapshot, _, readErr := manifests.Get(t.Context(), logical)
		var requestsJSON []byte
		dbErr := w.pool.QueryRow(t.Context(), `SELECT jsonb_agg(jsonb_build_object('id',id,'state',state,'current',current,'previous',plan->'previous','copies',plan->'copies','result_previous',result->'previous','error',error) ORDER BY created_at) FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2`, tenant, ref.Version).Scan(&requestsJSON)
		t.Fatalf("superseded public copy remains readable after replacement completed: HTTP%d; initial=%s stale=%s replacement=%s; manifest=%+v read=%v; requests=%s db=%v", response.StatusCode, initial, stale, replacement, snapshot, readErr, requestsJSON, dbErr)
	}
	after, _, err := manifests.Get(t.Context(), logical)
	must(t, err)
	current, found := after.Get("reader/one.webp")
	if !found {
		t.Fatal("replacement lost canonical reader file")
	}
	area, _, valid = layout.SplitBlobRef(current.Blob)
	if !valid || area != layout.AreaPrivate {
		t.Fatalf("replacement left canonical bytes in %q", current.Blob)
	}
	ring, err := token.NewRing(storage.key, nil)
	must(t, err)
	credential := ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), time.Now().Add(time.Minute))
	privateKey, err := item.Blob(current.Blob)
	must(t, err)
	response, err = http.Get(storage.gateway.URL + layout.URLPrefix + privateKey + "?t=" + credential)
	must(t, err)
	privateBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusOK || !bytes.Equal(privateBytes, publicBytes) {
		t.Fatal("replacement private item token failed to return the same reader bytes")
	}
	for _, delivery := range after.Deliveries {
		if delivery.ID == stale {
			t.Fatal("superseded public operation retained live storage pins")
		}
	}
}
