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

// Construction never dials. Writes require a successful capability probe;
// once qualified, storage outages still return ErrUnavailable.
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
	res := &resolver{verdicts: map[string]access.Resolution{}, anon: map[string]access.Resolution{}}
	res.set(cid(1), access.Resolution{Visible: true, Accessible: true})
	cfg2 := testConfig(env.Tenant, "acct")
	cfg2.Hooks = media.Hooks{Resolver: res}
	kinds, err := media.NewRegistry(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := media.NewManifests(store, kinds, media.ManifestOptions{Locker: media.PGLocker(pgtest.Pool(t, nil)), Journal: env.Journal()})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: ms,
		Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.test", SigningKey: signKey}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.StripPrefix("/media", reader.Handler(media.HandlerOptions{})))
	defer srv.Close()
	get := func() (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/media/gallery/" + cid(1))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Code string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Code
	}
	ctx := context.Background()
	ref := contentref.New(env.Tenant, "gallery", cid(1))
	name := ""
	edit := func() error {
		_, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			m.Files = []media.File{{Path: "originals/a.jpg", Blob: name, Type: "image/jpeg"}}
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
	if err := edit(); !errors.Is(err, media.ErrConditionalPutRequired) {
		t.Fatalf("edit before capability probe: %v", err)
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
	name, err = ms.NewBlob(ctx, ref, mustSum(blobOf([]byte("a"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := edit(); err != nil {
		t.Fatal(err)
	}
	if status, _ := get(); status != http.StatusOK {
		t.Fatalf("read after recovery: %d", status)
	}
	proxy.Down()
	if err := edit(); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("qualified edit while down: %v", err)
	}
	if status, code := get(); status != http.StatusServiceUnavailable || code != "unavailable" {
		t.Fatalf("qualified read while down: %d %q", status, code)
	}
	proxy.Up(t)
	if err := store.Check(ctx, env.Tenant+"/"); err != nil {
		t.Fatalf("health check once probed: %v", err)
	}
}
