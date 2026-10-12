package contentkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
)

// Post and poll images are ContentKit's own: the engine routes their media
// folders to the content module with no host hook. A draft post's images are
// its editors' until it is published; deleted, they are no one's.
func TestEngineDraftPostImagesIntegration(t *testing.T) {
	ctx := t.Context()
	env := mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	images := []string{"image/png"}
	folder := func(name string) media.Kind {
		return media.Kind{Name: name, Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 10}},
			Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}}
	}
	cfg := siteConfig(schema)
	cfg.Tenant = env.Tenant
	cfg.Media = &MediaConfig{
		Config:      media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{folder("post"), folder("poll")}},
		Delivery:    DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
		CommitLimit: RateLimitConfig{Disabled: true},
		ReadLimit:   RateLimitConfig{Disabled: true},
	}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Store: env.Store, Resolver: galleries{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	// Content writes queue media jobs: Start before serving.
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.Probes()) != 1 || e.Probes()[0].Check(ctx) != nil {
		t.Fatalf("media store probe %+v", e.Probes())
	}
	routes, err := e.Routes(Routes{Prefix: "/api/ck", Groups: ContentRoutes | UploadRoutes | MediaRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}

	var post content.Post
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/posts", content.PostInput{Title: ptr("Draft"), Body: ptr("text"), IsDraft: ptr(true)}, &post)
	ref := media.RefBody{Kind: "post", ID: post.ID}
	body := pngBytes(t, 16)
	sum := sha256.Sum256(body)
	var plan media.PresignReply
	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/media/upload/presign", media.PresignBody{Ref: ref, Path: "photo.png", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, &plan)
	s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/media/upload/presign", media.PresignBody{Ref: ref, Path: "photo.png", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, nil)
	req, _ := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	name := strings.TrimSuffix(plan.Path, ".png")
	var inline content.InlineImage
	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/posts/"+post.ID+"/images", content.ImageInput{Image: &name}, &inline)
	if inline.Ref != content.ImageRef(name) || inline.URL != nil {
		t.Fatalf("inline image %+v", inline)
	}

	var read media.ReadResult
	s.call(http.StatusOK, staff, "GET", "/api/ck/v1/media/post/"+post.ID+"?editor", nil, &read)
	if read.Access != media.AccessFull || len(read.Files) != 1 || read.Files[0].Path != plan.Path || !read.Files[0].Upload {
		t.Fatalf("editor read of the draft %+v", read)
	}
	s.call(http.StatusNotFound, anon, "GET", "/api/ck/v1/media/post/"+post.ID, nil, nil)
	s.call(http.StatusNotFound, user, "GET", "/api/ck/v1/media/post/"+post.ID+"?editor", nil, nil)

	s.call(http.StatusOK, staff, "PATCH", "/api/ck/v1/posts/"+post.ID, content.PostInput{IsDraft: ptr(false)}, nil)
	var viewer media.ReadResult
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/media/post/"+post.ID, nil, &viewer)
	if viewer.Access != media.AccessFull || viewer.Uploads != nil {
		t.Fatalf("anonymous read of the published post %+v", viewer)
	}
	s.call(http.StatusNoContent, staff, "DELETE", "/api/ck/v1/posts/"+post.ID, nil, nil)
	s.call(http.StatusNotFound, staff, "GET", "/api/ck/v1/media/post/"+post.ID+"?editor", nil, nil)
}
