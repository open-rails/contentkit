package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/tcpproxy"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// A host builds media with the bucket down: construction never dials, reads
// and edits fail with ErrUnavailable (503 over HTTP), and everything works
// once the bucket answers and Check has probed its capabilities.
func TestStoreOutage(t *testing.T) {
	env := s3test.Open(t)
	proxy := tcpproxy.New(t, env.Config.Endpoint)
	proxy.Down()
	cfg := env.Config
	cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = proxy.URL, "", nil
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := media.NewRegistry(media.Kind{Name: "post", Specs: map[string]media.Spec{"large": {}}})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.NewManifests(store, kinds, media.ManifestOptions{Locker: media.PGLocker(pgtest.Pool(t, nil))})
	if err != nil {
		t.Fatal(err)
	}
	res := &resolver{verdicts: map[string]access.Resolution{cid(1): {Visible: true, Accessible: true}}}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: ms, Kinds: kinds, Resolver: res,
		Delivery: media.Delivery{Mode: media.DeliverCookie, BaseURL: readBase, CookieDomain: "doujins.com", SigningKey: readKey}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/media", reader.Handler(media.HandlerOptions{Tenant: env.Tenant})))
	defer srv.Close()
	get := func() (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/media/post/" + cid(1))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Code string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Code
	}
	ctx := context.Background()
	ref := contentref.New(env.Tenant, "post", cid(1))
	edit := func() error {
		_, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			m.Files = []media.File{{Name: "a.jpg", Original: blobName("a"), Type: "image/jpeg"}}
			return nil
		})
		return err
	}

	if err := store.Check(ctx, env.Tenant+"/"); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("Check while down: %v", err)
	}
	if store.Capabilities() != (media.Capabilities{}) {
		t.Fatalf("capabilities before a probe: %+v", store.Capabilities())
	}
	if err := edit(); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("edit while down: %v", err)
	}
	if status, code := get(); status != http.StatusServiceUnavailable || code != "unavailable" {
		t.Fatalf("read while down: %d %q", status, code)
	}

	proxy.Up(t)
	if err := store.Check(ctx, env.Tenant+"/"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Capabilities(), env.Store.Capabilities(); got != want {
		t.Fatalf("probed capabilities %+v, want %+v", got, want)
	}
	if err := edit(); err != nil {
		t.Fatal(err)
	}
	if status, _ := get(); status != http.StatusOK {
		t.Fatalf("read after recovery: %d", status)
	}
	if err := store.Check(ctx, env.Tenant+"/"); err != nil {
		t.Fatalf("health check once probed: %v", err)
	}
}
