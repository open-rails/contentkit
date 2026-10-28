package critical_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// This proxy controls the real S3 protocol boundary. It never replaces the
// manifest store, journal, image processor or worker with an implementation fake.
type mediaWire struct {
	server              *httptest.Server
	target              *url.URL
	mu                  sync.Mutex
	hold                *mediaRequestHold
	webpPuts            int
	failManifest        bool
	failWebP            bool
	refuseOutput        func(*http.Request) bool
	refusedManifestPuts int
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
		refuse := w.failManifest && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "manifest.json")
		if refuse {
			w.refusedManifestPuts++
		}
		webp := r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Type"), "image/webp")
		failWebP := w.failWebP && webp
		if webp {
			w.webpPuts++
		}
		refuseOutput := w.refuseOutput != nil && w.refuseOutput(r)
		hold := w.hold
		if hold != nil && hold.match(r) {
			w.hold = nil
		} else {
			hold = nil
		}
		w.mu.Unlock()
		if failWebP || refuseOutput {
			out.Header().Set("Content-Type", "application/xml")
			out.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(out, `<Error><Code>AccessDenied</Code><Message>Owned fixture rendition write refused</Message></Error>`)
			return
		}
		if refuse {
			out.Header().Set("Content-Type", "application/xml")
			out.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(out, `<Error><Code>ServiceUnavailable</Code><Message>Owned fixture storage temporarily unavailable</Message></Error>`)
			return
		}
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

func TestMediaFailureRecovery(t *testing.T) {
	t.Run("CK-J11-S06", func(t *testing.T) {
		for _, profile := range []string{"storage-refusal", "database-settlement-abort"} {
			t.Run(profile, func(t *testing.T) {
				m := newResilientMedia(t)
				client := m.app.engine.Client()
				ref := galleryRef(t, client, "Recoverable media settlement")
				batch := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{stageFile(t, client, ref, "pages/one.png", imageBytes(t))}}
				restore := func() {}
				if profile == "storage-refusal" {
					m.wire.mu.Lock()
					m.wire.failManifest = true
					m.wire.mu.Unlock()
					restore = func() { m.wire.mu.Lock(); m.wire.failManifest = false; m.wire.mu.Unlock() }
				} else {
					// Real PostgreSQL aborts only this accepted operation's
					// settlement. No manifest, quota or job row is fabricated.
					function := pgx.Identifier{m.schema, "critical_media_settlement_refusal"}.Sanitize()
					table := pgx.Identifier{m.schema, "content_media_commits"}.Sanitize()
					_, err := m.pool.Exec(t.Context(), "CREATE FUNCTION "+function+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation_id='"+batch.OperationID+"'::uuid AND NEW.state='applied' THEN RAISE EXCEPTION 'owned media settlement refusal'; END IF; RETURN NEW; END $$; CREATE TRIGGER critical_media_settlement_refusal BEFORE UPDATE ON "+table+" FOR EACH ROW EXECUTE FUNCTION "+function+"()")
					must(t, err)
					restore = func() {
						_, err := m.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS critical_media_settlement_refusal ON "+table+"; DROP FUNCTION IF EXISTS "+function+"()")
						must(t, err)
					}
				}
				t.Cleanup(restore)
				_, err := client.PostMediaUploadCommit(t.Context(), batch)
				var failure *ck.Error
				if !errors.As(err, &failure) || failure.Status < 500 {
					t.Fatalf("settlement fault was not observable: %v", err)
				}
				if profile == "storage-refusal" {
					m.wire.mu.Lock()
					refused := m.wire.refusedManifestPuts
					m.wire.mu.Unlock()
					if refused == 0 {
						t.Fatal("storage fault was never exercised")
					}
				}
				restore()
				// Reopen the actual runtime: retry cannot rely on its old cache.
				m.app.server.Close()
				must(t, m.app.engine.Close(t.Context()))
				m.app.engine, err = ck.New(t.Context(), m.app.cfg, m.app.deps)
				must(t, err)
				m.app.server = mountEngine(t, m.app.engine)
				must(t, m.app.engine.StartJobs(t.Context()))
				client = m.app.engine.Client()
				_, err = client.PostMediaUploadCommit(t.Context(), batch)
				must(t, err)
				m.start(t)
				ready := awaitMediaReady(t, client, ref)
				requireMediaPaths(t, ready, "pages/one.png", "reader/one.webp")
				_, err = client.PostMediaUploadCommit(t.Context(), batch)
				must(t, err)
				after := awaitMediaReady(t, client, ref)
				requireMediaPaths(t, after, "pages/one.png", "reader/one.webp")
				for _, file := range after.Files {
					if file.Path == "reader/one.webp" {
						assertImage(t, file.URL, 48, 32)
					}
				}
			})
		}
		t.Run("resource-failure-is-visible-and-regeneration-recovers", func(t *testing.T) {
			m := newResilientMedia(t)
			client := m.app.engine.Client()
			ref := galleryRef(t, client, "Image resource refusal")
			child := m.start(t, func(c *mediaChildConfig) { c.MaxPixels = 64 })
			_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
			must(t, err)
			var status media.ReadResult
			waitFor(t, func() bool {
				var err error
				status, err = client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
				must(t, err)
				return status.Status != nil && status.Status.FailureCount == 1
			})
			if status.Status.State != media.StateFailed || len(status.Status.Failures) != 1 {
				t.Fatalf("failed worker not reported: %+v", status.Status)
			}
			failure := status.Status.Failures[0]
			if failure.Path != "pages/one.png" || failure.Code != media.CodeImageTooLarge {
				t.Fatalf("resource refusal lost its source/code: %+v", failure)
			}
			if strings.Contains(failure.Message, m.schema) || strings.Contains(failure.Message, m.storage.bucket) || strings.Contains(failure.Message, "http://") {
				t.Fatalf("editor status leaked storage internals: %+v", failure)
			}
			// A persisted image failure can precede the job's final transaction.
			// This case changes worker limits; the separate crash cases deliberately
			// kill in-flight jobs and exercise their rescue path.
			queue := m.queue(t)
			waitFor(t, func() bool {
				jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()).States(
					rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled).First(1))
				must(t, err)
				return len(jobs.Jobs) == 0
			})
			child.kill(t)
			m.start(t)
			_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRegenerate}}})
			must(t, err)
			ready := awaitMediaReady(t, client, ref)
			requireMediaPaths(t, ready, "pages/one.png", "reader/one.webp")
			if ready.Status == nil || ready.Status.FailureCount != 0 {
				t.Fatalf("successful regeneration retained a failure: %+v", ready.Status)
			}
		})
	})
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

func newResilientMedia(t *testing.T, configure ...func(*ck.Config)) *resilientMedia {
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
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 4}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "attachments/{name}", Types: []string{"text/plain"}, MaxBytes: 80 << 20, Max: 2}},
		Private: []media.Private{{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{16, 32}, Image: media.Image{Aspect: media.Square}}},
	}}}, WorkerSchema: w.schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: m.signing, TTL: time.Minute, Window: time.Second}}
	for _, change := range configure {
		change(&a.cfg)
	}
	a.deps.Store = store
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := media.NewRegistry(config, media.Hooks{})
	must(t, err)
	must(t, a.engine.StartJobs(t.Context()))
	t.Cleanup(func() {
		a.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.engine.Close(ctx); err != nil {
			t.Errorf("stop media host before storage cleanup: %v", err)
		}
	})
	return &resilientMedia{world: w, app: a, storage: m, wire: wire, config: worker.Config{Pool: w.pool, Schema: a.cfg.Media.WorkerSchema, ContentSchema: w.schema, HostSchema: w.schema + "_river", Store: store, Kinds: registry, Threads: 1, IngestWorkers: 1, ImageSources: 1, DerivativeWorkers: 1, ShutdownGrace: time.Second}}
}

func TestMediaClientResumption(t *testing.T) {
	t.Run("CK-J11-S07", func(t *testing.T) {
		t.Run("Go_ingest_retains_its_exact_accepted_batch_and_wrapped_cause", func(t *testing.T) {
			m := newResilientMedia(t)
			user, credential := m.person(t, 0)
			authtest.GrantRole(t, m.tenant[0], m.app.group, iam.UserSubject(user.ID), m.writer)
			client := m.app.client(credential)
			ref := galleryRef(t, client, "Resumed Go media commit")
			base, err := url.Parse(m.app.server.URL)
			must(t, err)
			proxy := httputil.NewSingleHostReverseProxy(base)
			var lost atomic.Bool
			wire := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/media/upload/commit") || !lost.CompareAndSwap(false, true) {
					proxy.ServeHTTP(out, r)
					return
				}
				request, err := http.NewRequestWithContext(r.Context(), r.Method, base.String()+r.URL.RequestURI(), r.Body)
				if err != nil {
					http.Error(out, "fixture request failed", 500)
					return
				}
				request.Header = r.Header.Clone()
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					http.Error(out, "fixture upstream failed", 500)
					return
				}
				defer response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
				if response.StatusCode != http.StatusOK {
					t.Errorf("commit was not accepted before response loss: %d", response.StatusCode)
				}
				connection, _, err := out.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
			}))
			defer wire.Close()
			uncertain := ck.NewClient(wire.URL+"/content", &http.Client{Transport: bearerTransport{credential, http.DefaultTransport}})
			operation := uuid.NewString()
			meta := map[string]any{"caption": "original", "nested": map[string]any{"label": "retained"}}
			_, err = uncertain.IngestMedia(t.Context(), nil, ref, operation, []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t)), Meta: meta}})
			var unconfirmed *ck.MediaCommitError
			if !errors.As(err, &unconfirmed) || !lost.Load() {
				t.Fatalf("actual accepted commit uncertainty not retained: %v", err)
			}
			cause := unconfirmed.Unwrap()
			if cause == nil || cause != unconfirmed.Err || !errors.Is(err, cause) {
				t.Fatalf("real ingest error lost wrapped cause: %v", err)
			}
			if !strings.Contains(unconfirmed.Error(), "media commit was not confirmed") || !strings.Contains(unconfirmed.Error(), cause.Error()) {
				t.Fatal("uncertain ingest error lost its actionable message or original cause")
			}
			if unconfirmed.Commit.OperationID != operation || unconfirmed.Commit.Ref != ref || len(unconfirmed.Commit.Ops) != 1 {
				t.Fatalf("retry identity changed: %+v", unconfirmed.Commit)
			}
			meta["caption"] = "changed-after-error"
			meta["nested"].(map[string]any)["label"] = "mutated"
			if unconfirmed.Commit.Ops[0].Meta["caption"] != "original" || unconfirmed.Commit.Ops[0].Meta["nested"].(map[string]any)["label"] != "retained" {
				t.Fatal("retained retry batch aliases caller-owned metadata")
			}
			_, err = client.PostMediaUploadCommit(t.Context(), unconfirmed.Commit)
			must(t, err)
			m.start(t)
			ready := awaitMediaReady(t, client, ref)
			requireMediaPaths(t, ready, "pages/one.png", "reader/one.webp")
			for _, file := range ready.Files {
				if file.Path == "pages/one.png" && file.Meta["caption"] != "original" {
					t.Fatalf("retry changed original metadata: %+v", file.Meta)
				}
			}
		})
		t.Run("browser_queue_controls_multipart_resume_and_commit_identity", func(t *testing.T) {
			if _, err := os.Stat("../../sdk/ui/dist/client.js"); err != nil {
				t.Fatal("build the shipped SDK before the selected media browser contract")
			}
			m := newResilientMedia(t, func(c *ck.Config) {
				c.Media.Config.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
			})
			user, credential := m.person(t, 0)
			authtest.GrantRole(t, m.tenant[0], m.app.group, iam.UserSubject(user.ID), m.writer)
			ref := galleryRef(t, m.app.client(credential), "Browser queue resilience")
			base, err := url.Parse(m.app.server.URL)
			must(t, err)
			startWorker, started := make(chan struct{}, 1), make(chan struct{})
			mux := http.NewServeMux()
			mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
			mux.Handle("/bundle/", http.StripPrefix("/bundle/", http.FileServer(http.Dir("../../sdk/ui/dist"))))
			mux.Handle("/noble/", http.StripPrefix("/noble/", http.FileServer(http.Dir("../../sdk/ui/node_modules/@noble/hashes"))))
			png := imageBytes(t)
			mux.HandleFunc("/fixture.png", func(out http.ResponseWriter, _ *http.Request) {
				out.Header().Set("Content-Type", "image/png")
				_, _ = out.Write(png)
			})
			mux.HandleFunc("POST /start-worker", func(out http.ResponseWriter, r *http.Request) {
				select {
				case startWorker <- struct{}{}:
				case <-r.Context().Done():
					return
				}
				select {
				case <-started:
					out.WriteHeader(http.StatusNoContent)
				case <-r.Context().Done():
				}
			})
			mux.HandleFunc("/", func(out http.ResponseWriter, _ *http.Request) {
				out.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(out, `<!doctype html><script type="importmap">{"imports":{"@noble/hashes/":"/noble/"}}</script><script type="module">import {createContentKitClient,ItemUploadQueue} from "/bundle/client.js"; window.mediaFactory={createContentKitClient,ItemUploadQueue};</script>`)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			fixture, err := json.Marshal(map[string]any{"url": server.URL, "token": credential, "ref": ref})
			must(t, err)
			command := exec.CommandContext(t.Context(), "node", "browser/media-resilience.mjs")
			command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			must(t, command.Start())
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			for {
				select {
				case <-startWorker:
					m.start(t)
					close(started)
				case err := <-done:
					if err != nil {
						t.Fatalf("shipped media browser failed: %v\n%s", err, output.String())
					}
					var receipt struct {
						Status   string   `json:"status"`
						Bindings []string `json:"bindings"`
					}
					must(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &receipt))
					if receipt.Status != "passed" || len(receipt.Bindings) != 11 {
						t.Fatalf("browser did not execute queue contract: %s", output.String())
					}
					ready := awaitMediaReady(t, m.app.engine.Client(), ref)
					requireMediaPaths(t, ready, "pages/two.png", "pages/renamed.png", "attachments/book.txt", "cover.png", "reader/two.webp", "reader/renamed.webp")
					for _, file := range ready.Files {
						if file.Path != "attachments/book.txt" {
							continue
						}
						response, err := http.Get(file.URL)
						must(t, err)
						hash := sha256.New()
						size, err := io.Copy(hash, response.Body)
						response.Body.Close()
						must(t, err)
						if response.StatusCode != 200 || size != 65<<20 || hex.EncodeToString(hash.Sum(nil)) != "293ac2ec51c4b810c496f147ebba14e5916f360a27cab012e7d8eccd1946f8ef" {
							t.Fatalf("resumed multipart bytes differ: status=%d size=%d digest=%x", response.StatusCode, size, hash.Sum(nil))
						}
					}
					return
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
			}
		})
	})
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

type mediaChildConfig struct {
	Kinds                                                                  media.Config
	Schema, ContentSchema, HostSchema, Endpoint, Bucket, Ready, AfterImage string
	MaxPixels                                                              int
}

// The public River hook observes a successful worker result after its actual
// manifest checkpoint and before queue acknowledgement. Killing the process
// here leaves the real queue row running; no application result is fabricated.
type mediaAfterImage struct {
	river.HookDefaults
	path string
	once sync.Once
}

func (h *mediaAfterImage) WorkEnd(ctx context.Context, job *rivertype.JobRow, workErr error) error {
	if workErr != nil || job.Kind != (workqueue.DerivativeArgs{}).Kind() {
		return workErr
	}
	park := false
	h.once.Do(func() { park = true })
	if !park {
		return workErr
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := os.WriteFile(h.path, data, 0600); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestMediaWorkerProcess(t *testing.T) {
	path := os.Getenv("CONTENTKIT_CRITICAL_MEDIA_CHILD")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	must(t, err)
	var c mediaChildConfig
	must(t, json.Unmarshal(data, &c))
	pool, err := pgxpool.New(t.Context(), os.Getenv("CONTENTKIT_CRITICAL_PG"))
	must(t, err)
	defer pool.Close()
	store, err := medias3.New(medias3.Config{Bucket: c.Bucket, Endpoint: c.Endpoint, AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	must(t, err)
	registry, err := media.NewRegistry(c.Kinds, media.Hooks{})
	must(t, err)
	config := worker.Config{Pool: pool, Schema: c.Schema, ContentSchema: c.ContentSchema, HostSchema: c.HostSchema, Store: store, Kinds: registry, Threads: 1, IngestWorkers: 1, ImageSources: 1, DerivativeWorkers: 1, ShutdownGrace: time.Second, MaxPixels: c.MaxPixels}
	if c.AfterImage != "" {
		config.RiverHooks = []rivertype.Hook{&mediaAfterImage{path: c.AfterImage}}
	}
	w, err := worker.New(t.Context(), config)
	must(t, err)
	must(t, os.WriteFile(c.Ready, []byte("configured"), 0600))
	must(t, w.Run(t.Context()))
}

type mediaChild struct {
	command *exec.Cmd
	done    chan error
	stopped bool
}

func (m *resilientMedia) start(t *testing.T, configure ...func(*mediaChildConfig)) *mediaChild {
	t.Helper()
	dir := t.TempDir()
	c := mediaChildConfig{Kinds: m.config.Kinds.Config(), Schema: m.config.Schema, ContentSchema: m.config.ContentSchema, HostSchema: m.config.HostSchema, Endpoint: m.wire.server.URL, Bucket: m.storage.bucket, Ready: filepath.Join(dir, "ready")}
	for _, change := range configure {
		change(&c)
	}
	data, err := json.Marshal(c)
	must(t, err)
	config := filepath.Join(dir, "config.json")
	must(t, os.WriteFile(config, data, 0600))
	binary, err := os.Executable()
	must(t, err)
	logPath := filepath.Join(dir, "worker.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	must(t, err)
	t.Cleanup(func() { _ = log.Close() })
	command := exec.Command(binary, "-test.run=^TestMediaWorkerProcess$", "-test.timeout=4m", "-test.v")
	command.Env = append(os.Environ(), "CONTENTKIT_CRITICAL_MEDIA_CHILD="+config, "GOMAXPROCS="+maxProcs())
	command.Stdout, command.Stderr = log, log
	must(t, command.Start())
	child := &mediaChild{command: command, done: make(chan error, 1)}
	go func() { child.done <- command.Wait() }()
	t.Cleanup(func() {
		child.kill(t)
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("owned media worker: %s", data)
		}
	})
	waitForWithin(t, 30*time.Second, func() bool {
		select {
		case err := <-child.done:
			child.stopped = true
			data, _ := os.ReadFile(logPath)
			t.Fatalf("media worker exited before configuration: %v\n%s", err, data)
		default:
		}
		_, err := os.Stat(c.Ready)
		return err == nil
	})
	return child
}

func maxProcs() string {
	if value := os.Getenv("GOMAXPROCS"); value != "" {
		return value
	}
	return "2"
}

func (c *mediaChild) kill(t *testing.T) {
	t.Helper()
	if c.stopped {
		return
	}
	must(t, c.command.Process.Kill())
	select {
	case err := <-c.done:
		c.stopped = true
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("worker did not die by SIGKILL: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owned media worker failed to terminate")
	}
}

func (m *resilientMedia) queue(t *testing.T) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(m.pool), &river.Config{Schema: m.config.Schema})
	must(t, err)
	return client
}

type mediaRedelivery struct {
	kind string
	raw  json.RawMessage
}

func (r mediaRedelivery) Kind() string                 { return r.kind }
func (r mediaRedelivery) MarshalJSON() ([]byte, error) { return r.raw, nil }

func replayMediaJob(t *testing.T, queue *river.Client[pgx.Tx], original *rivertype.JobRow) int64 {
	t.Helper()
	job, err := queue.Insert(t.Context(), mediaRedelivery{original.Kind, original.EncodedArgs}, &river.InsertOpts{Queue: original.Queue, MaxAttempts: original.MaxAttempts})
	must(t, err)
	return job.Job.ID
}

func settledPreparedOutputs(t *testing.T, m *resilientMedia, client *ck.Client, ref media.RefBody) map[string]string {
	t.Helper()
	waitFor(t, func() bool {
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
		must(t, err)
		return read.Status != nil && read.Status.State == media.StateReady && read.Status.FailureCount == 0
	})
	read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	if len(read.Files) != 0 {
		t.Fatal("prepared draft outputs escaped through the ordinary reader")
	}
	// Observe the persisted worker checkpoint directly. Editor reads would
	// request extra render work, while an ordinary reader must not expose drafts.
	logical, err := m.config.Kinds.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := m.config.Kinds.Item(logical)
	must(t, err)
	journal, err := media.NewPGJournal(m.pool, m.schema, nil)
	must(t, err)
	manifests, err := media.NewManifests(m.storage.store, m.config.Kinds, media.ManifestOptions{Locker: media.PGLocker(m.pool), Journal: journal})
	must(t, err)
	snapshot, _, err := manifests.Get(t.Context(), logical)
	must(t, err)
	out := map[string]string{}
	for _, file := range snapshot.Files {
		if file.IsUpload() || file.Blob == "" || file.Gone {
			continue
		}
		key, err := item.Blob(file.Blob)
		must(t, err)
		object, err := m.storage.store.Head(t.Context(), key)
		must(t, err)
		if object.Size != file.Size || object.ContentType != file.Type {
			t.Fatalf("checkpoint output %s differs from its actual stored object", file.Path)
		}
		out[file.Path] = file.Blob
	}
	if out["reader/one.webp"] == "" {
		t.Fatal("checkpoint contains no persisted reader rendition")
	}
	return out
}

func TestMediaWorkerRecovery(t *testing.T) {
	t.Run("exhausted-image-job-records-failure-and-cancels", func(t *testing.T) {
		m := newResilientMedia(t)
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Rendition retries exhausted")
		m.wire.mu.Lock()
		m.wire.failWebP = true
		m.wire.mu.Unlock()
		m.start(t)
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		queue := m.queue(t)
		var finished *rivertype.JobRow
		waitForWithin(t, 30*time.Second, func() bool {
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()))
			must(t, err)
			for _, job := range jobs.Jobs {
				if job.State == rivertype.JobStateRetryable {
					_, err := queue.JobRetry(t.Context(), job.ID)
					must(t, err)
				}
				if job.State == rivertype.JobStateCancelled || job.State == rivertype.JobStateDiscarded || job.State == rivertype.JobStateCompleted {
					finished = job
					return true
				}
			}
			return false
		})
		if finished.State != rivertype.JobStateCancelled || finished.Attempt != workqueue.MaxAttempts {
			t.Fatalf("exhausted image job: state=%s attempt=%d errors=%+v", finished.State, finished.Attempt, finished.Errors)
		}
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
		must(t, err)
		if read.Status == nil || read.Status.State != media.StateFailed || read.Status.FailureCount != 1 || len(read.Status.Failures) != 1 || read.Status.Failures[0].Path != "pages/one.png" {
			t.Fatalf("exhausted rendition remained pending: %+v", read.Status)
		}
		logical, err := m.config.Kinds.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		journal, err := media.NewPGJournal(m.pool, m.schema, nil)
		must(t, err)
		manifests, err := media.NewManifests(m.storage.store, m.config.Kinds, media.ManifestOptions{Locker: media.PGLocker(m.pool), Journal: journal})
		must(t, err)
		snapshot, _, err := manifests.Get(t.Context(), logical)
		must(t, err)
		page, ok := snapshot.Get("pages/one.png")
		if !ok || page.Fail() == nil || len(page.Pending) != 0 {
			t.Fatalf("page failure was not persisted: %+v", page)
		}
	})
	t.Run("CK-J11-S04_process_death_before_output_checkpoint_retries_one_visible_rendition", func(t *testing.T) {
		m := newResilientMedia(t)
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Worker interrupted before output")
		hold := m.wire.pause(t, func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Type"), "image/webp")
		})
		child := m.start(t)
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		select {
		case <-hold.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("worker did not reach output storage boundary")
		}
		queue := m.queue(t)
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()).States(rivertype.JobStateRunning))
		must(t, err)
		if len(jobs.Jobs) != 1 {
			t.Fatalf("expected actual executing image job, got %d", len(jobs.Jobs))
		}
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if read.State != media.StateProcessing {
			t.Fatalf("unwritten rendition claimed ready: %+v", read)
		}
		child.kill(t)
		select {
		case <-hold.canceled:
		case <-time.After(10 * time.Second):
			t.Fatal("killed worker left output request live")
		}
		// Accepted pending work also survives a host Engine.Close/New cycle;
		// the borrowed database and original queued payload remain authoritative.
		m.app.server.Close()
		must(t, m.app.engine.Close(t.Context()))
		m.app.engine, err = ck.New(t.Context(), m.app.cfg, m.app.deps)
		must(t, err)
		m.app.server = mountEngine(t, m.app.engine)
		must(t, m.app.engine.StartJobs(t.Context()))
		client = m.app.engine.Client()
		persisted, err := queue.JobGet(t.Context(), jobs.Jobs[0].ID)
		must(t, err)
		if !bytes.Equal(persisted.EncodedArgs, jobs.Jobs[0].EncodedArgs) {
			t.Fatal("host reopen lost the original accepted media job")
		}
		m.start(t)
		replay := replayMediaJob(t, queue, jobs.Jobs[0])
		ready := awaitMediaReady(t, client, ref)
		requireMediaPaths(t, ready, "pages/one.png", "reader/one.webp")
		waitFor(t, func() bool {
			job, err := queue.JobGet(t.Context(), replay)
			must(t, err)
			return job.State == rivertype.JobStateCompleted
		})
		for _, file := range ready.Files {
			if file.Path == "reader/one.webp" {
				assertImage(t, file.URL, 48, 32)
			}
		}
		_, err = m.app.client("").GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J11-S05_process_death_after_checkpoint_preserves_immutable_output_without_reencoding", func(t *testing.T) {
		m := newResilientMedia(t)
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Worker interrupted after output")
		checkpoint := filepath.Join(t.TempDir(), "finished-image.json")
		child := m.start(t, func(c *mediaChildConfig) { c.AfterImage = checkpoint })
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		var original rivertype.JobRow
		waitFor(t, func() bool {
			data, err := os.ReadFile(checkpoint)
			return err == nil && json.Unmarshal(data, &original) == nil
		})
		queue := m.queue(t)
		row, err := queue.JobGet(t.Context(), original.ID)
		must(t, err)
		if row.State != rivertype.JobStateRunning {
			t.Fatalf("checkpoint was not before queue acknowledgement: %s", row.State)
		}
		paths := settledPreparedOutputs(t, m, client, ref)
		m.wire.mu.Lock()
		writes := m.wire.webpPuts
		m.wire.mu.Unlock()
		if writes == 0 {
			t.Fatal("checkpoint contains no real image output")
		}
		child.kill(t)
		m.start(t)
		for range 2 {
			replay := replayMediaJob(t, queue, &original)
			waitFor(t, func() bool {
				job, err := queue.JobGet(t.Context(), replay)
				must(t, err)
				return job.State == rivertype.JobStateCompleted
			})
		}
		got := settledPreparedOutputs(t, m, client, ref)
		for path, blob := range paths {
			if got[path] != blob {
				t.Fatalf("checkpoint output %s was replaced: %s -> %s", path, blob, got[path])
			}
		}
		m.wire.mu.Lock()
		afterWrites := m.wire.webpPuts
		m.wire.mu.Unlock()
		if afterWrites != writes {
			t.Fatalf("replayed completed output was reencoded: writes %d -> %d", writes, afterWrites)
		}
	})
}
