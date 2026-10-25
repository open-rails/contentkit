package critical_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/worker"
)

// This proxy controls the real S3 protocol boundary. It never replaces the
// manifest store, journal, image processor or worker with an implementation fake.
type mediaWire struct {
	server   *httptest.Server
	target   *url.URL
	mu       sync.Mutex
	hold     *mediaRequestHold
	webpPuts int
}

type heldMediaRequest struct {
	method, path, host string
	header             http.Header
	body               []byte
}

type mediaRequestHold struct {
	match             func(*http.Request) bool
	arrived           chan heldMediaRequest
	release, canceled chan struct{}
	once              sync.Once
}

func (h *mediaRequestHold) unblock() { h.once.Do(func() { close(h.release) }) }

func newMediaWire(t *testing.T) *mediaWire {
	t.Helper()
	target, err := url.Parse(os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"))
	must(t, err)
	w := &mediaWire{target: target}
	proxy := httputil.NewSingleHostReverseProxy(target)
	w.server = httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		if r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Type"), "image/webp") {
			w.webpPuts++
		}
		hold := w.hold
		if hold != nil && hold.match(r) {
			w.hold = nil
		} else {
			hold = nil
		}
		w.mu.Unlock()
		if hold != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(out, "owned wire read failed", 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hold.arrived <- heldMediaRequest{method: r.Method, path: r.URL.RequestURI(), host: r.Host, header: r.Header.Clone(), body: body}
			select {
			case <-hold.release:
			case <-r.Context().Done():
				close(hold.canceled)
				return
			}
		}
		proxy.ServeHTTP(out, r)
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *mediaWire) pause(t *testing.T, match func(*http.Request) bool) *mediaRequestHold {
	t.Helper()
	h := &mediaRequestHold{match: match, arrived: make(chan heldMediaRequest, 1), release: make(chan struct{}), canceled: make(chan struct{})}
	w.mu.Lock()
	w.hold = h
	w.mu.Unlock()
	t.Cleanup(h.unblock)
	return h
}

func (w *mediaWire) deliver(t *testing.T, held heldMediaRequest) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), held.method, w.target.String()+held.path, bytes.NewReader(held.body))
	must(t, err)
	req.Host, req.Header = held.host, held.header.Clone()
	response, err := http.DefaultClient.Do(req)
	must(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	must(t, err)
	if response.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale real S3 write returned %d: %s", response.StatusCode, body)
	}
	return response.StatusCode
}

type resilientMedia struct {
	*world
	app     *application
	storage *mediaWorld
	wire    *mediaWire
	config  worker.Config
}

func newResilientMedia(t *testing.T) *resilientMedia {
	t.Helper()
	w := newWorld(t)
	a := w.apps[0]
	m := newMediaStorage(t, *a.cfg.Scope)
	wire := newMediaWire(t)
	store, err := medias3.New(medias3.Config{Bucket: m.bucket, Endpoint: wire.server.URL, PublicEndpoint: os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	must(t, err)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Media = &ck.MediaConfig{Config: media.Config{BaseURL: m.gateway.URL, Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 4}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Private: []media.Private{{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{16, 32}, Image: media.Image{Aspect: media.Square}}},
	}}}, WorkerSchema: w.schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: m.signing, TTL: time.Minute, Window: time.Second}}
	a.deps.Store = store
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := media.NewRegistry(config)
	must(t, err)
	must(t, a.engine.Start(t.Context()))
	return &resilientMedia{world: w, app: a, storage: m, wire: wire, config: worker.Config{Pool: w.pool, Schema: a.cfg.Media.WorkerSchema, ContentSchema: w.schema, HostSchema: w.schema + "_river", Store: store, Kinds: registry, Threads: 1, ImageWorkers: 1, ImageSources: 1, VideoWorkers: 1, ShutdownGrace: time.Second}}
}

func TestMediaConcurrentRevision(t *testing.T) {
	t.Run("CK-J11-S03_a_late_stale_manifest_cannot_overwrite_the_committed_winner", func(t *testing.T) {
		m := newResilientMedia(t)
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Concurrent media revision")
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMeta, Meta: map[string]any{"revision": "initial"}}}})
		must(t, err)
		hold := m.wire.pause(t, func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "manifest.json")
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := client.PostMediaUploadCommit(ctx, media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMeta, Meta: map[string]any{"revision": "stale"}}}})
			done <- err
		}()
		var delayed heldMediaRequest
		select {
		case delayed = <-hold.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("first conditional manifest write never reached real S3 boundary")
		}
		if delayed.header.Get("If-Match") == "" {
			t.Fatal("revision edit did not carry its storage precondition")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("abandoned edit unexpectedly succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("abandoned edit did not return")
		}
		select {
		case <-hold.canceled:
		case <-time.After(10 * time.Second):
			t.Fatal("abandoned storage request stayed live")
		}
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMeta, Meta: map[string]any{"revision": "winner"}}}})
		must(t, err)
		m.wire.deliver(t, delayed)
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if read.Meta["revision"] != "winner" {
			t.Fatalf("stale write replaced winner: %+v", read.Meta)
		}
	})
}
