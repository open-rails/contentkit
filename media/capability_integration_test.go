package media_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// Every Manifests takes the Locker, so processes that have and have not
// probed the store serialize; once probed, edits also write with If-Match, so
// a writer outside the lock (a stale process, an operator) is not overwritten.
func TestManifestsLockAndUseIfMatchOnceProbed(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	kinds, err := media.NewRegistry(media.Kind{Name: "post"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{}); err == nil {
		t.Fatal("lock-free Manifests must be refused")
	}
	locker := media.PGLocker(pgtest.Pool(t, nil))
	probed, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: locker})
	if err != nil {
		t.Fatal(err)
	}
	unprobed, err := media.NewManifests(env.WithCapabilities(t, media.Capabilities{}), kinds, media.ManifestOptions{Locker: locker})
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
			ms = unprobed
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
			if _, err := unprobedRaw(t, env).Edit(ctx, ref, func(m *media.Manifest) error { set(m, "outside"); return nil }); err != nil {
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

// unprobedRaw is a process with its own lock space (not the host's), so its
// edits do not wait on the caller's lock.
func unprobedRaw(t *testing.T, env *s3test.Env) *media.Manifests {
	kinds, _ := media.NewRegistry(media.Kind{Name: "post"})
	ms, err := media.NewManifests(env.Store, kinds, media.ManifestOptions{Locker: noLock{}})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

type noLock struct{}

func (noLock) Lock(context.Context, string) (func(), error) { return func() {}, nil }

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
