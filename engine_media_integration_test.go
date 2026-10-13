package contentkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
)

// A content item's images are its content-level media item's, decided by the
// catalog with no host hook: a draft's are its editors' until it is
// published; deleted, they are no viewer's.
func TestEngineDraftContentImagesIntegration(t *testing.T) {
	ctx := t.Context()
	env := mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	images := []string{"image/png"}
	folder := func(name string) media.Kind {
		return media.Kind{Name: name, Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 10}},
			Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}}
	}
	blog := folder("blog")
	blog.Uploads[0].Content = true
	blog.Uploads = append(blog.Uploads, media.Upload{Path: "files/{name}", Types: images, MaxBytes: 1 << 20})
	cfg := siteConfig(schema)
	cfg.Tenant = env.Tenant
	cfg.Media = &MediaConfig{
		Config:      media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{blog, folder("poll")}},
		Delivery:    DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
		CommitLimit: RateLimitConfig{Disabled: true},
		ReadLimit:   RateLimitConfig{Disabled: true},
	}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Store: env.Store})
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

	var post catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Draft", Body: "text"}}}, &post)
	ref := media.RefBody{Kind: "blog", ID: post.ID}
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
	var inline catalog.InlineImage
	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+post.ID+"/images", content.ImageInput{Image: &name}, &inline)
	if inline.Ref != catalog.ImageRef(name) || inline.URL != nil {
		t.Fatalf("inline image %+v", inline)
	}

	var read media.ReadResult
	s.call(http.StatusOK, staff, "GET", "/api/ck/v1/media/blog/"+post.ID+"?editor", nil, &read)
	if read.Access != media.AccessFull || len(read.Files) != 1 || read.Files[0].Path != plan.Path || !read.Files[0].Upload {
		t.Fatalf("editor read of the draft %+v", read)
	}
	s.call(http.StatusNotFound, anon, "GET", "/api/ck/v1/media/blog/"+post.ID, nil, nil)
	s.call(http.StatusNotFound, user, "GET", "/api/ck/v1/media/blog/"+post.ID+"?editor", nil, nil)

	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	var viewer media.ReadResult
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/media/blog/"+post.ID, nil, &viewer)
	if viewer.Uploads != nil {
		t.Fatalf("anonymous read of the published item %+v", viewer)
	}
	s.call(http.StatusNoContent, staff, "DELETE", "/api/ck/v1/contents/"+post.ID, nil, nil)
	s.call(http.StatusNotFound, anon, "GET", "/api/ck/v1/media/blog/"+post.ID, nil, nil)
	s.call(http.StatusNotFound, user, "GET", "/api/ck/v1/media/blog/"+post.ID+"?editor", nil, nil)
}

// A content item's body images are its body item's ({kind}.body): nobody who
// has not opened a locked item reads or is signed them, its buyer is signed
// the opened images through the item token, and an open item's show anyone.
func TestEngineLockedBodyImagesIntegration(t *testing.T) {
	ctx := t.Context()
	env := mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	images := []string{"image/png"}
	blog := media.Kind{Name: "blog", Uploads: []media.Upload{{Path: "{name}", Content: true, Types: images, MaxBytes: 1 << 20, Named: true, Max: 10},
		{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
		Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}}
	key := "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))
	cfg := siteConfig(schema)
	cfg.Tenant = env.Tenant
	cfg.Content.Tiers, cfg.Content.TokenClaims = []string{"premium"}, []string{"premium"}
	cfg.Media = &MediaConfig{Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{blog}},
		Delivery: DeliveryConfig{SigningKey: key}, CommitLimit: RateLimitConfig{Disabled: true}, ReadLimit: RateLimitConfig{Disabled: true}}
	auth := testauth.Auth{Authz: staffAuthz{}, Claims: map[string][]string{"buyer": {"premium"}}}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: auth, Store: env.Store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	routes, err := e.Routes(Routes{Prefix: "/api/ck", Groups: ContentRoutes | UploadRoutes | MediaRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, buyer, user := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "buyer", Kind: "user", IP: "10.0.0.3"}, access.Actor{ID: "u1", Kind: "user", IP: "10.0.0.4"}
	anon := access.Actor{Anonymous: true, IP: "10.0.0.9"}
	ring := func() token.Ring {
		k, err := token.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		r, err := token.NewRing(k, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()

	// post creates a published blog post with one body image, the worker's
	// rendering of it stood in for by a private image over the upload's blob.
	post := func(rule access.Rule) (catalog.Content, string) {
		t.Helper()
		var c catalog.Content
		s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/contents", catalog.ContentInput{Kind: "blog", Access: &rule,
			Text: map[string]catalog.Text{"en": {Title: "Post", Body: "text"}}}, &c)
		ref := media.RefBody{Kind: "blog" + media.BodySuffix, ID: c.ID}
		s.call(http.StatusNotFound, user, "GET", "/api/ck/v1/media/"+ref.Kind+"/"+c.ID+"?editor", nil, nil)
		body := pngBytes(t, 16)
		sum := sha256.Sum256(body)
		presign := media.PresignBody{Ref: ref, Path: "photo.png", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
		s.call(http.StatusForbidden, user, "POST", "/api/ck/v1/media/upload/presign", presign, nil)
		var plan media.PresignReply
		s.call(http.StatusOK, staff, "POST", "/api/ck/v1/media/upload/presign", presign, &plan)
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
		var inline catalog.InlineImage
		s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+c.ID+"/images", content.ImageInput{Image: &name}, &inline)
		item := contentref.New(env.Tenant, ref.Kind, c.ID)
		if _, err := e.media.jobs.Manifests().Place(ctx, item); err != nil {
			t.Fatal(err)
		}
		if _, err := e.media.jobs.Manifests().EditExisting(ctx, item, func(m *media.Manifest) error {
			i := m.Find(plan.Path)
			if i < 0 || m.Files[i].Blob == "" {
				return fmt.Errorf("image not placed: %+v", m.Files)
			}
			m.Files[i].Pending = nil
			m.Files = append(m.Files, media.File{Path: "inline-open/" + name + ".webp", Blob: m.Files[i].Blob, Type: "image/webp", Size: 1,
				From: plan.Path, Preset: "inline-open"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.call(http.StatusOK, staff, "PUT", "/api/ck/v1/contents/"+c.ID+"/text/en", catalog.Text{Title: "Post", Body: "see " + inline.Ref}, nil)
		s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
		return c, name
	}
	// signed checks a body shows name through the item token.
	signed := func(c catalog.Content, name string) {
		t.Helper()
		u, ok := c.Images[name]
		if !ok || !strings.Contains(c.Body, u) {
			t.Fatalf("body image %s not shown: %+v", name, c)
		}
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		k := strings.TrimPrefix(parsed.Path, "/v1/")
		if !strings.HasPrefix(k, env.Tenant+"/blog.body/"+c.ID+"/private/") || ring.VerifyPrivate(parsed.Query().Get("t"), k, time.Now()) != nil {
			t.Fatalf("body image URL %s is not the item's signed private file", u)
		}
	}

	locked, name := post(access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}})
	for _, who := range []access.Actor{anon, user} {
		var c catalog.Content
		s.call(http.StatusOK, who, "GET", "/api/ck/v1/contents/"+locked.ID, nil, &c)
		if c.Body != "" || len(c.Images) != 0 {
			t.Fatalf("a locked viewer is shown the body: %+v", c)
		}
		s.call(http.StatusNotFound, who, "GET", "/api/ck/v1/media/blog.body/"+locked.ID, nil, nil)
	}
	var opened catalog.Content
	s.call(http.StatusOK, buyer, "GET", "/api/ck/v1/contents/"+locked.ID, nil, &opened)
	signed(opened, name)
	var read media.ReadResult
	s.call(http.StatusOK, buyer, "GET", "/api/ck/v1/media/blog.body/"+locked.ID, nil, &read)
	if read.Access != media.AccessFull || len(read.Public) != 0 {
		t.Fatalf("the buyer's read %+v", read)
	}
	// The locked item has no public files: anonymous exposure hides it.
	m, _, err := e.media.jobs.Manifests().Get(ctx, contentref.New(env.Tenant, "blog.body", locked.ID))
	if err != nil || !m.Hidden {
		t.Fatalf("locked body item exposed: hidden=%v %v", m != nil && m.Hidden, err)
	}

	// Made public, anyone opens it.
	s.call(http.StatusOK, staff, "PATCH", "/api/ck/v1/contents/"+locked.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.Public}}, nil)
	var public catalog.Content
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/contents/"+locked.ID, nil, &public)
	signed(public, name)
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/media/blog.body/"+locked.ID, nil, nil)
}
