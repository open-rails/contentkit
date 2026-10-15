package contentkit

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
)

// A catalog key may preserve old database identities while its media lives in
// the tenant/app partition. Both inline-image writes and body reads must use
// that media partition; standalone legacy catalog bindings still work.
func TestEngineBodyImagesUseRegisteredMediaScope(t *testing.T) {
	for _, mode := range []string{"scoped", "legacy", "legacy binding fallback"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			env := mediatest.Open(t)
			pool := pgtest.Pool(t, nil)
			cfg := siteConfig(engineSchema(t, pool))
			cfg.Tenant = env.Tenant
			if mode == "scoped" {
				cfg.Scope = &Scope{TenantID: NewTenantID(), AppID: NewAppID()}
				cfg.LegacyCatalogKey, cfg.Tenant = env.Tenant, ""
				prefix := "tenant/" + cfg.Scope.TenantID.String() + "/"
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
					defer stop()
					for object, err := range env.Store.List(cleanup, prefix) {
						if err != nil {
							t.Error(err)
							break
						}
						if err := env.Store.Delete(cleanup, object.Key); err != nil {
							t.Error(err)
						}
					}
				})
			}
			key := "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))
			cfg.Media = &MediaConfig{
				Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{{Name: "blog",
					Uploads: []media.Upload{{Path: "{name}", Body: true, Types: []string{"image/png"}, MaxBytes: 1 << 20, Named: true, Max: 10},
						{Path: "files/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
					Private: []media.Private{{Name: "inline", From: "{name}", To: "inline/{name}.webp", Image: &media.Image{Width: 800}}}}}},
				Delivery: DeliveryConfig{SigningKey: key}, CommitLimit: RateLimitConfig{Disabled: true}, ReadLimit: RateLimitConfig{Disabled: true},
			}
			deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Store: env.Store}
			if cfg.Scope != nil {
				deps.Authorization = scopeTestAuthorization{}
			}
			e, err := New(ctx, cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close(context.Background()) })
			if err := e.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "legacy binding fallback" {
				if err := e.catalog.BindMedia(catalog.Media{Reader: e.media.reader, Versions: e.media.versions}); err != nil {
					t.Fatal(err)
				}
			}
			client := e.Client()
			post, err := client.PostContents(ctx, catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Post", Body: "text"}}})
			if err != nil {
				t.Fatal(err)
			}
			vid := post.Versions[0].ID
			uploaded, err := client.IngestMedia(ctx, nil, media.RefBody{Kind: "blog", ID: post.ID, Version: vid}, uuid.NewString(),
				[]MediaFile{{Path: "photo.png", Type: "image/png", Body: bytes.NewReader(pngBytes(t, 16))}})
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(path.Base(uploaded.Files[0].Path), path.Ext(uploaded.Files[0].Path))
			inline, err := client.PostContentImages(ctx, post.ID, catalog.BodyImageInput{Image: &name, Version: vid})
			if err != nil {
				t.Fatalf("body-image preview must use the registered media scope: %v", err)
			}
			if inline.Ref != catalog.ImageRef(name) {
				t.Fatalf("inline reference %+v", inline)
			}
			ref, err := e.media.reg.VersionRef("blog", post.ID, vid)
			if err != nil {
				t.Fatal(err)
			}
			foreign := contentref.New("foreign", "blog", post.ID).WithVersion(vid)
			if _, err := e.media.reader.Read(ctx, foreign, access.Actor{ID: "staff", Kind: "user"}, media.ReadOptions{Editor: true}); err == nil {
				t.Fatal("explicit foreign namespace was accepted")
			}
			// Stand in only for image encoding, keeping the real journal, registry,
			// storage, authorization and private-file signer in the request path.
			if _, err := e.media.jobs.Manifests().Place(ctx, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := e.media.jobs.Manifests().EditExisting(ctx, ref, func(m *media.Manifest) error {
				i := m.Find(uploaded.Files[0].Path)
				if i < 0 || m.Files[i].Blob == "" {
					return fmt.Errorf("uploaded image was not placed")
				}
				m.Files[i].Pending = nil
				m.Files = append(m.Files, media.File{Path: "inline/" + name + ".webp", Blob: m.Files[i].Blob, Type: "image/webp", Size: 1,
					From: uploaded.Files[0].Path, Preset: "inline"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.PutContentText(ctx, post.ID, "en", catalog.Text{Title: "Post", Body: "see " + inline.Ref}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.PostContentPublish(ctx, post.ID, catalog.PublishInput{}); err != nil {
				t.Fatal(err)
			}
			routes, err := e.Routes(Routes{Prefix: "/api/ck", Groups: ContentRoutes})
			if err != nil {
				t.Fatal(err)
			}
			var shown catalog.Content
			serve(t, routes).call(http.StatusOK, access.Actor{Anonymous: true, IP: "10.0.0.9"}, "GET", "/api/ck/v1/contents/"+post.ID, nil, &shown)
			signed := shown.Images[name]
			if signed == "" || !strings.Contains(shown.Body, signed) {
				t.Fatalf("body rendering must sign the registered version's image: %+v", shown)
			}
			u, err := url.Parse(signed)
			if err != nil {
				t.Fatal(err)
			}
			item, err := e.media.reg.Item(ref)
			if err != nil {
				t.Fatal(err)
			}
			object := strings.TrimPrefix(u.Path, "/v1/")
			parsedKey, err := token.ParseKey(key)
			if err != nil {
				t.Fatal(err)
			}
			ring, err := token.NewRing(parsedKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(object, item.Prefix()+"private/") || ring.VerifyPrivate(u.Query().Get("t"), object, time.Now()) != nil {
				t.Fatalf("body image is not signed for its exact registered folder: %s", signed)
			}
		})
	}
}
