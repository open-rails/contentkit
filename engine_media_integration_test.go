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
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
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
	blog.Uploads[0].Body = true
	blog.Uploads = append(blog.Uploads, media.Upload{Path: "files/{name}", Types: images, MaxBytes: 1 << 20})
	blog.Public, blog.Private = nil, []media.Private{{Name: "inline", From: "{name}", To: "inline/{name}.webp", Image: &media.Image{Width: 800}}}
	cfg := siteConfig(schema)
	cfg.Tenant = env.Tenant
	blogKind := cfg.Content.Kinds["blog"]
	blogKind.Entities = []string{"tag"}
	cfg.Content.Kinds["blog"] = blogKind
	cfg.Content.ContentFilters.Domains, cfg.Content.ContentFilters.AppliesTo = []kinds.Domain{
		{Name: "main", Hosts: []string{"example.com"}}, {Name: "safe", Hosts: []string{"safe.example"}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "restricted"}, Visible: false}}},
	}, []string{"blog"}
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
	routes, err := e.Routes(Routes{Prefix: "/api/ck", Groups: ContentRoutes | UploadRoutes | MediaRoutes | EntityRoutes})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff, user, anon := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true, IP: "10.0.0.9"}

	var post catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Draft", Body: "text"}}}, &post)
	vid := post.Versions[0].ID
	ref := media.RefBody{Kind: "blog", ID: post.ID, Version: vid}
	item := "/api/ck/v1/media/blog/" + post.ID + "/versions/" + vid
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
	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+post.ID+"/images", catalog.BodyImageInput{Image: &name, Version: vid}, &inline)
	if inline.Ref != catalog.ImageRef(name) || inline.URL != nil {
		t.Fatalf("inline image %+v", inline)
	}

	var read media.ReadResult
	s.call(http.StatusOK, staff, "GET", item+"?editor", nil, &read)
	if read.Access != media.AccessFull || len(read.Files) != 1 || read.Files[0].Path != plan.Path || !read.Files[0].Upload {
		t.Fatalf("editor read of the draft %+v", read)
	}
	s.call(http.StatusNotFound, anon, "GET", item, nil, nil)
	s.call(http.StatusNotFound, user, "GET", item+"?editor", nil, nil)

	s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	var viewer media.ReadResult
	s.call(http.StatusOK, anon, "GET", item, nil, &viewer)
	if viewer.Uploads != nil {
		t.Fatalf("anonymous read of the published item %+v", viewer)
	}
	// An internal classifier is a fixed domain exclusion even with the
	// default user_toggle setting. Media must obey it independently of UI.
	var markers []entities.Entity
	s.call(http.StatusCreated, staff, "POST", "/api/ck/v1/entities", []entities.EntityInput{{
		Kind: "tag", Slug: "restricted", Internal: true,
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Restricted"}},
	}}, &markers)
	s.call(http.StatusOK, staff, "PUT", "/api/ck/v1/contents/"+post.ID+"/entities", entities.ContentEntities{
		Memberships: []entities.Membership{{EntityID: markers[0].ID}},
	}, nil)
	s.call(http.StatusOK, anon, "GET", "http://example.com"+item, nil, nil)
	for _, actor := range []access.Actor{anon, user, staff} {
		s.call(http.StatusNotFound, actor, "GET", "http://safe.example"+item, nil, nil)
	}
	s.call(http.StatusNoContent, staff, "DELETE", "/api/ck/v1/contents/"+post.ID+"/versions/"+vid, nil, nil)
	s.call(http.StatusNotFound, anon, "GET", item, nil, nil)
	s.call(http.StatusNotFound, user, "GET", item+"?editor", nil, nil)
}

// A content item's body images are its version's private files: nobody who
// has not opened a locked version reads or is signed them, while its buyer,
// and anyone once it is free, is signed them through the item token.
func TestEngineLockedBodyImagesIntegration(t *testing.T) {
	ctx := t.Context()
	env := mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	images := []string{"image/png"}
	blog := media.Kind{Name: "blog", Uploads: []media.Upload{{Path: "{name}", Body: true, Types: images, MaxBytes: 1 << 20, Named: true, Max: 10},
		{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
		Private: []media.Private{{Name: "inline", From: "{name}", To: "inline/{name}.webp", Image: &media.Image{Width: 800}}}}
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
		vid := c.Versions[0].ID
		ref := media.RefBody{Kind: "blog", ID: c.ID, Version: vid}
		s.call(http.StatusNotFound, user, "GET", "/api/ck/v1/media/"+ref.Kind+"/"+c.ID+"/versions/"+vid+"?editor", nil, nil)
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
		s.call(http.StatusOK, staff, "POST", "/api/ck/v1/contents/"+c.ID+"/images", catalog.BodyImageInput{Image: &name, Version: vid}, &inline)
		item := contentref.New(env.Tenant, ref.Kind, c.ID).WithVersion(vid)
		if _, err := e.media.jobs.Manifests().Place(ctx, item); err != nil {
			t.Fatal(err)
		}
		if _, err := e.media.jobs.Manifests().EditExisting(ctx, item, func(m *media.Manifest) error {
			i := m.Find(plan.Path)
			if i < 0 || m.Files[i].Blob == "" {
				return fmt.Errorf("image not placed: %+v", m.Files)
			}
			m.Files[i].Pending = nil
			m.Files = append(m.Files, media.File{Path: "inline/" + name + ".webp", Blob: m.Files[i].Blob, Type: "image/webp", Size: 1,
				From: plan.Path, Preset: "inline"})
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
		if !strings.HasPrefix(k, env.Tenant+"/blog/"+c.Versions[0].ID+"/private/") || ring.VerifyPrivate(parsed.Query().Get("t"), k, time.Now()) != nil {
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
		var read media.ReadResult
		s.call(http.StatusOK, who, "GET", "/api/ck/v1/media/blog/"+locked.ID+"/versions/"+locked.Versions[0].ID, nil, &read)
		if read.Access == media.AccessFull || len(read.Files) != 0 || read.Cookie != nil {
			t.Fatalf("a locked viewer reads the body's files: %+v", read)
		}
	}
	var opened catalog.Content
	s.call(http.StatusOK, buyer, "GET", "/api/ck/v1/contents/"+locked.ID, nil, &opened)
	signed(opened, name)
	var read media.ReadResult
	s.call(http.StatusOK, buyer, "GET", "/api/ck/v1/media/blog/"+locked.ID+"/versions/"+locked.Versions[0].ID, nil, &read)
	if read.Access != media.AccessFull || len(read.Public) != 0 {
		t.Fatalf("the buyer's read %+v", read)
	}

	// Made public, anyone opens it.
	s.call(http.StatusOK, staff, "PATCH", "/api/ck/v1/contents/"+locked.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.Public}}, nil)
	var public catalog.Content
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/contents/"+locked.ID, nil, &public)
	signed(public, name)
	var free media.ReadResult
	s.call(http.StatusOK, anon, "GET", "/api/ck/v1/media/blog/"+locked.ID+"/versions/"+locked.Versions[0].ID, nil, &free)
	if free.Access != media.AccessFull {
		t.Fatalf("anyone's read of the free version %+v", free)
	}
}

// Engine maintenance keeps old drafts and their media. A deleted channel's
// media is hidden, kept for a restore through the retention, then purged once.
// A post with a matured public audience release has its media exposed.
func TestEngineDraftAndEntityRetentionIntegration(t *testing.T) {
	ctx := t.Context()
	env := mediatest.Open(t)
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	cfg.Tenant = env.Tenant
	cfg.Content.EntityKinds["channel"] = kinds.EntityKind{Route: "c", Owner: "user"}
	cfg.Media = &MediaConfig{
		Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{{Name: "blog",
			Uploads: []media.Upload{{Path: "{name}", Body: true, Types: []string{"image/png"}, MaxBytes: 1 << 20, Named: true, Max: 10},
				{Path: "files/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "inline", From: "{name}", To: "inline/{name}.webp", Image: &media.Image{Width: 800}}}}}},
		Delivery: DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
	}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Store: env.Store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(ctx) })
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	jobs := func(kind, id string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema + "_river", "river_job"}.Sanitize()+
			` WHERE kind = $1 AND args::text LIKE '%' || $2 || '%'`, kind, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	staff := testauth.Actor(access.Actor{ID: "staff", Kind: "user"}, staffAuthz{})
	ids, err := e.catalog.Create(ctx, staff, []catalog.ContentInput{{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Unfinished"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var draftVersion string
	if err := pool.QueryRow(ctx, `SELECT id FROM `+table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2`, env.Tenant, ids[0]).Scan(&draftVersion); err != nil {
		t.Fatal(err)
	}
	draftRef, err := e.media.reg.VersionRef("blog", ids[0], draftVersion)
	if err != nil {
		t.Fatal(err)
	}
	draftItem, err := e.media.reg.Item(draftRef)
	if err != nil {
		t.Fatal(err)
	}
	// Give the version a manifest incarnation without starting image processing.
	draftManifest, err := e.media.jobs.Manifests().Edit(ctx, draftRef, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "Unfinished"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table("content_items")+` SET created_at = now() - interval '1 year' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if n, err := e.catalog.PurgeDeleted(ctx); err != nil || n != 0 {
		t.Fatalf("purged %d (%v); an old draft must stay", n, err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM `+table("content_items")+` WHERE tenant_id=$1 AND id=$2`, env.Tenant, ids[0]).Scan(&state); err != nil || state != catalog.StateDraft {
		t.Fatalf("old draft state=%q, err=%v", state, err)
	}
	keptManifest, _, err := e.media.jobs.Manifests().Get(ctx, draftRef)
	if err != nil || keptManifest.Incarnation != draftManifest.Incarnation {
		t.Fatalf("old draft's media changed: manifest=%+v, err=%v", keptManifest, err)
	}
	var deletions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema + "_river", "river_job"}.Sanitize()+`
WHERE kind='contentkit_media_delete_folder' AND args->>'prefix'=$1`, draftItem.Prefix()).Scan(&deletions); err != nil {
		t.Fatal(err)
	}
	if deletions != 0 {
		t.Fatalf("old draft queued %d media deletions", deletions)
	}

	passed := time.Now().Add(-time.Minute)
	freed, err := e.catalog.Create(ctx, staff, []catalog.ContentInput{{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Freed"}},
		Access: &access.Rule{Level: access.PPV, Releases: []access.Release{{At: passed, Level: access.Public}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.catalog.Publish(ctx, staff, []catalog.Publish{{ContentID: freed[0]}}); err != nil {
		t.Fatal(err)
	}
	exposed := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema + "_river", "river_job"}.Sanitize()+
			` WHERE kind = 'contentkit_media_expose' AND args->'ref'->>'content_kind' = $1 AND args->'ref'->>'content_id' = $2`, "blog", freed[0]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := exposed()
	if n, err := e.catalog.PublishDue(ctx); err != nil || n != 0 || exposed() != before {
		t.Fatalf("released %d (%v); exposes %d after %d", n, err, exposed(), before)
	}

	made, err := e.entities.CreateEntities(ctx, []entities.EntityInput{{Kind: "channel", Slug: "studio", OwnerUserID: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	ch := made[0].ID
	if _, err := e.entities.DeleteEntities(ctx, []entities.EntityID{ch}); err != nil {
		t.Fatal(err)
	}
	if jobs("contentkit_media_expose", string(ch)) != 0 || jobs("contentkit_media_delete_folder", string(ch)) != 0 {
		t.Fatal("deleting the channel queued exposure or deletion")
	}
	if n, err := e.entities.PurgeFolders(ctx, 100); err != nil || n != 0 {
		t.Fatalf("purged %d (%v) within the retention", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table("content_entities")+` SET deleted_at = now() - interval '31 days' WHERE id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	if n, err := e.entities.PurgeFolders(ctx, 100); err != nil || n != 1 || jobs("contentkit_media_delete_folder", string(ch)) != 1 {
		t.Fatalf("purged %d (%v) past the retention", n, err)
	}
	if n, err := e.entities.PurgeFolders(ctx, 100); err != nil || n != 0 {
		t.Fatalf("purged again %d (%v)", n, err)
	}
	active := entities.StateActive
	if _, err := e.entities.UpdateEntities(ctx, []entities.EntityUpdate{{ID: ch, State: &active}}); err != nil {
		t.Fatal(err)
	}
	var deletedAt, purgedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at, media_purged_at FROM `+table("content_entities")+` WHERE id = $1`, ch).Scan(&deletedAt, &purgedAt); err != nil ||
		deletedAt != nil || purgedAt != nil {
		t.Fatalf("restored: deleted_at %v, media_purged_at %v, %v", deletedAt, purgedAt, err)
	}
}
