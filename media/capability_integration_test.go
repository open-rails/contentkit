package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// The lock orders cooperating writers; If-Match also protects against a
// writer outside that lock (for example a process whose session died).
func TestManifestsLockAndUseIfMatch(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	kinds := miniRegistry(t, env.Tenant)
	if _, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{}); err == nil {
		t.Fatal("lock-free Manifests must be refused")
	}
	locker := media.PGLocker(pgtest.Pool(t, nil))
	probed, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker})
	if err != nil {
		t.Fatal(err)
	}
	other, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := contentref.New(env.Tenant, "post", cid(7))
	set := func(m *media.Manifest, k string) {
		if m.Meta == nil {
			m.Meta = map[string]any{}
		}
		m.Meta[k] = true
	}
	var wg sync.WaitGroup
	for i := range 8 {
		ms := probed
		if i%2 == 1 {
			ms = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error { set(m, fmt.Sprint("k", i)); return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// A writer outside the lock lands between the probed edit's read and write.
	var once atomic.Bool
	if _, err := probed.Edit(ctx, ref, func(m *media.Manifest) error {
		if once.CompareAndSwap(false, true) {
			if _, err := outsideLock(t, env).Edit(ctx, ref, func(m *media.Manifest) error { set(m, "outside"); return nil }); err != nil {
				return err
			}
		}
		set(m, "inside")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := probed.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "outside", "inside"} {
		if got.Meta[k] != true {
			t.Fatalf("edit %s was lost: %v", k, got.Meta)
		}
	}
}

// outsideLock is a process with its own lock space (not the host's), so its
// edits do not wait on the caller's lock.
func outsideLock(t *testing.T, env *s3test.Env) *media.Manifests {
	ms, err := media.NewManifests(env.Store, miniRegistry(t, env.Tenant), media.ManifestOptions{Locker: noLock{}})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

type noLock struct{}

func (noLock) Lock(context.Context, string) (func(), error) { return func() {}, nil }

func TestManifestsRefuseUnconditionalWrites(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t)
	reg := miniRegistry(t, env.Tenant)
	locker := media.PGLocker(pgtest.Pool(t, nil))
	ms, err := media.NewManifests(env.WithCapabilities(t, media.Capabilities{}), reg, media.ManifestOptions{Locker: locker})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New(env.Tenant, "post", cid(7))
	for _, tc := range []struct {
		name string
		edit func(context.Context, contentref.ContentRef, func(*media.Manifest) error) (*media.Manifest, error)
	}{
		{name: "create or edit", edit: ms.Edit},
		{name: "worker edit", edit: ms.EditExisting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.edit(t.Context(), ref, func(*media.Manifest) error {
				t.Error("callback ran without a storage fence")
				return nil
			})
			if !errors.Is(err, media.ErrConditionalPutRequired) {
				t.Fatalf("unqualified write: %v", err)
			}
		})
	}
	item, _ := reg.Item(ref)
	if _, err := env.Store.Head(t.Context(), item.ManifestKey()); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("unqualified create wrote a manifest: %v", err)
	}
}

func TestUploadsRefuseUnconditionalStorageBeforeMovingBytes(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t).WithoutConditionalPut(t)
	f := newFixtureOn(t, env, nil)
	pool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, t.Context(), pool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: f.ms, Queue: f.q, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	ref := f.ref("gallery", 1)
	data := []byte("must not be uploaded")
	sum := sha256.Sum256(data)
	if _, err := up.Presign(t.Context(), f.editor, media.PresignRequest{
		Ref: ref, Path: "originals/1.png", Type: "image/png", Size: int64(len(data)), SHA256: sum[:],
	}); !errors.Is(err, media.ErrConditionalPutRequired) {
		t.Fatalf("unqualified presign: %v", err)
	}
	body := bytes.NewReader(data)
	if _, err := up.Ingest(t.Context(), f.editor, media.IngestRequest{
		Ref: ref, Path: "originals/1.png", Type: "image/png", Size: int64(len(data)), Body: body,
	}); !errors.Is(err, media.ErrConditionalPutRequired) || body.Len() != len(data) {
		t.Fatalf("unqualified ingest: %v, %d unread bytes", err, body.Len())
	}
	ops := []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "refused"}}}
	if _, err := up.Commit(t.Context(), f.editor, ref, ops); !errors.Is(err, media.ErrConditionalPutRequired) {
		t.Fatalf("unqualified commit: %v", err)
	}
	used, reserved, err := limiter.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || used != 0 || reserved != 0 {
		t.Fatalf("unqualified storage consumed quota: used=%d reserved=%d err=%v", used, reserved, err)
	}
	item, _ := f.reg.Item(ref)
	for obj, err := range env.Store.List(t.Context(), item.Prefix()) {
		t.Fatalf("unqualified storage wrote %s: %v", obj.Key, err)
	}
	if jobs := f.q.take(); len(jobs) != 0 {
		t.Fatalf("unqualified commit queued processing: %+v", jobs)
	}
	payload, err := json.Marshal(media.CommitBody{Ref: media.RefBody{Kind: "gallery", ID: ref.ContentID}, Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	handler := media.UploadHandler(up, media.UploadHandlerOptions{Actor: func(*http.Request) (access.Actor, bool) { return f.editor, true }})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/commit", bytes.NewReader(payload)))
	var reply media.ErrorReply
	if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil || res.Code != http.StatusServiceUnavailable || reply.Code != media.CodeUnavailable {
		t.Fatalf("unqualified HTTP commit: %d %s, %v", res.Code, res.Body.String(), err)
	}
}

func TestManifestRevisionFencesReturningToEarlierContent(t *testing.T) {
	t.Parallel()
	env := s3test.Open(t)
	ms := s3test.Manifests(t, env.Store, miniRegistry(t, env.Tenant), media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "post", cid(7))
	set := func(value string) (*media.Manifest, error) {
		return ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
			m.Meta = map[string]any{"title": value}
			return nil
		})
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, etag, err := ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set("second"); err != nil {
		t.Fatal(err)
	}
	item, _ := ms.Registry().Item(ref)
	rc, _, err := env.Store.Get(t.Context(), item.ManifestKey(), media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, lastETag, err := ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// The delayed edit was prepared against the first state, before a newer
	// writer changed the item and returned it to identical JSON.
	_, err = env.Store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(body), int64(len(body)), media.PutOptions{IfMatch: etag})
	if !errors.Is(err, media.ErrPreconditionFailed) {
		t.Fatalf("late PUT matching an earlier state: %v", err)
	}
	current, _, err := ms.Get(t.Context(), ref)
	if err != nil || current.Meta["title"] != "first" {
		t.Fatalf("late PUT changed the newer content: %+v, %v", current, err)
	}
	if _, err := set("first"); err != nil {
		t.Fatal(err)
	}
	_, unchangedETag, err := ms.Get(t.Context(), ref)
	if err != nil || unchangedETag != lastETag {
		t.Fatalf("semantic no-op changed ETag: %q, %v", unchangedETag, err)
	}
}

// A transient failure during the capability probe (here a 429 on the
// If-None-Match create) must not be recorded as "no conditional PUT".
func TestProbeDoesNotRecordTransientFailures(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	target, _ := url.Parse(env.Config.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var throttled, healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() && r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*" && strings.Contains(r.URL.Path, "probe-") &&
			throttled.CompareAndSwap(false, true) {
			proxy.ServeHTTP(w, r) // the first create lands
			return
		}
		if !healthy.Load() && r.Method == http.MethodPut && r.Header.Get("If-None-Match") == "*" && throttled.Load() {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`<Error><Code>SlowDown</Code></Error>`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := env.Config
	cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = srv.URL, "", nil
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Check(ctx, env.Tenant+"/"); err == nil {
		t.Fatalf("a throttled probe succeeded and recorded %+v", store.Capabilities())
	}
	if store.Capabilities() != (media.Capabilities{}) {
		t.Fatalf("a throttled probe recorded %+v", store.Capabilities())
	}
	healthy.Store(true)
	if err := store.Check(ctx, env.Tenant+"/"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Capabilities(), env.Store.Capabilities(); got != want {
		t.Fatalf("probed %+v after recovery, want %+v", got, want)
	}
}

// Capabilities a host declares (even all false, as on Ceph RGW) are never
// re-probed.
func TestDeclaredCapabilitiesAreNotReprobed(t *testing.T) {
	env := s3test.Open(t)
	store := env.WithCapabilities(t, media.Capabilities{})
	if err := store.Check(context.Background(), env.Tenant+"/"); err != nil {
		t.Fatal(err)
	}
	if store.Capabilities() != (media.Capabilities{}) {
		t.Fatalf("declared capabilities re-probed: %+v", store.Capabilities())
	}
}

// Ceph RGW ignores If-None-Match: * (the create overwrites, so the If-Match
// with the first ETag then answers 412). That backend has no conditional PUT,
// and the probe must say so instead of failing forever.
func TestProbeAcceptsABackendIgnoringIfNoneMatch(t *testing.T) {
	env := s3test.Open(t)
	target, _ := url.Parse(env.Config.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	bucketPrefix := "/" + env.Config.Bucket + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" || !strings.HasPrefix(r.URL.Path, bucketPrefix) {
			proxy.ServeHTTP(w, r)
			return
		}
		// As RGW: If-None-Match is ignored and the PUT overwrites.
		body, _ := io.ReadAll(r.Body)
		obj, err := env.Store.Put(r.Context(), strings.TrimPrefix(r.URL.Path, bucketPrefix), bytes.NewReader(body), int64(len(body)), media.PutOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", obj.ETag)
	}))
	defer srv.Close()
	cfg := env.Config
	cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = srv.URL, "", nil
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background(), env.Tenant+"/"); err != nil {
		t.Fatalf("probe against an If-None-Match-ignoring backend: %v", err)
	}
	if caps := store.Capabilities(); caps.ConditionalPut || caps.ChecksumSHA256 != env.Store.Capabilities().ChecksumSHA256 {
		t.Fatalf("capabilities %+v; want no conditional PUT, checksum as the backend", caps)
	}
}
