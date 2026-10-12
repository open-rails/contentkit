package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/taxonomy"
)

const (
	tenant = "site"
	prefix = "/api/contentkit"
)

// world is a host: AuthKit with root roles holding ContentKit's permissions,
// ContentKit's schema, the media stack over MinIO, and the host's resolver
// and upload hooks.
type world struct {
	t      *testing.T
	ak     *authkit.Client
	scope  auth.Scope // the root group: AuthKit's root scope, where staff hold their permissions
	media  iam.Perm   // edit any item's media
	staff  iam.Role
	pool   *pgxpool.Pool
	schema string
	env    *mediatest.Env
	reg    *media.Registry
	jobs   *media.Jobs
	queue  *workqueue.Queue
	up     *media.Uploads
	rd     *media.Reader
	tax    *taxonomy.Store
	router *contenturl.Router
	owner  string // the gallery's owner, when a test has one
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := t.Context()
	w := &world{t: t}
	rbac := authkit.NewRoles(authkit.APIKeys)
	// ContentKit names its staff permissions; the host declares them and
	// builds its roles from them, beside its own.
	w.media = rbac.Root.Permission("media", "edit")
	grants := []iam.Grant{w.media}
	for _, p := range rbac.Root.Declare(access.Names(access.Permissions()...)...) {
		grants = append(grants, p)
	}
	w.staff = rbac.Root.Role("staff", grants...)
	w.ak, _ = authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))
	var err error
	if w.scope, err = w.ak.Scope(ctx, iam.RootGroup()); err != nil {
		t.Fatal(err)
	}

	w.pool = pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, w.pool)
	w.schema = pgtest.EmptySchema(t, ctx, w.pool)
	db := stdlib.OpenDBFromPool(w.pool)
	t.Cleanup(func() { _ = db.Close() })
	if err := contentkit.Migrate(ctx, contentkit.MigrateConfig{DB: db, Schema: w.schema}); err != nil {
		t.Fatal(err)
	}
	w.env = mediatest.Open(t)
	images := []string{"image/png"}
	if w.reg, err = media.NewRegistry(media.Config{Namespace: w.env.Tenant, BaseURL: "https://media.test",
		Kinds: []media.Kind{
			{Name: "gallery", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "originals/{name}", Types: images, MaxBytes: 1 << 20, Max: 10}, {Path: "cover", Types: images, MaxBytes: 1 << 20},
					{Path: "extras/{name}", Types: images, MaxBytes: 1 << 20, Max: 1}},
				Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{200}, Image: media.Image{MinWidth: 8}}}},
			{Name: "book", Versions: true, KeepOriginals: true,
				Publish: &media.Publish{Pages: "pages/{name}", Renditions: []string{"read"}, Download: "zip"},
				Uploads: []media.Upload{{Path: "pages/{name}", Types: images, MaxBytes: 1 << 20}},
				Private: []media.Private{{Name: "read", From: "pages/{name}", To: "read/{name}.webp", Image: &media.Image{Width: 64}},
					{Name: "zip", To: "download/pages.zip", Zip: "read/", HostOnly: true}}},
			{Name: "user", Namespace: "accounts", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "avatar", Types: images, MaxBytes: 1 << 20}},
				Public: []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{64, 128},
					Image: media.Image{Aspect: media.Square}, Default: "avatar.png"}}},
		},
		Avatars: media.Avatars{Kind: "user", Preset: "avatar"},
		Hooks:   media.Hooks{Resolver: w, CanUpload: w}}); err != nil {
		t.Fatal(err)
	}
	var journal *media.PGJournal
	journal, w.queue = w.env.Processing(w.reg)
	if w.jobs, err = media.NewJobs(media.JobsConfig{Store: w.env.Store, Registry: w.reg, Locker: mediatest.Locker(t, w.env.Store),
		Journal: journal, Processes: w.queue, Pool: w.env.Pool()}); err != nil {
		t.Fatal(err)
	}
	key := token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}
	ring, err := token.NewRing(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.up, err = media.NewUploads(media.UploadOptions{Store: w.env.Store, Manifests: w.jobs.Manifests(), Tickets: &ring, Commits: media.RateLimit{Disabled: true}}); err != nil {
		t.Fatal(err)
	}
	if w.rd, err = media.NewReader(media.ReaderOptions{Manifests: w.jobs.Manifests(), Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: key}}); err != nil {
		t.Fatal(err)
	}
	if w.tax, err = taxonomy.New(taxonomy.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: []string{"tag"}, Languages: []string{"en"},
		Listing: &taxonomy.Listing{Kinds: []string{"gallery"}, Eligibility: search.Eligibility{SQL: `SELECT 'free'::text AS access`},
			Access: access.Columns{Level: "h.access", TierName: "premium", Levels: map[string]access.Level{"free": access.Public, "premium": access.TierLevel}}}}); err != nil {
		t.Fatal(err)
	}
	return w
}

// runtime is the host's one ContentKit: Auth and UserInfo are AuthKit's,
// passed straight in; staff hold ContentKit's permissions in the root group.
func (w *world) runtime(edit func(*contentkit.RuntimeConfig)) *contentkit.Runtime {
	w.t.Helper()
	var rt *contentkit.Runtime
	urls, err := contenturl.New(contenturl.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant})
	if err != nil {
		w.t.Fatal(err)
	}
	router, err := contenturl.NewRouter(urls, contenturl.RouterOptions{
		Routes: contenturl.Routes{"post": "blog", "tag": "tag"}, Languages: []string{"en"},
		Visibility: func(r *http.Request, l contenturl.Link) (contenturl.Visibility, error) {
			if l.ContentKind == content.KindPost {
				return rt.Content.PostVisibility(r.Context(), l.ContentID)
			}
			return contenturl.Visible, nil
		}})
	if err != nil {
		w.t.Fatal(err)
	}
	catalog, err := w.tax.CatalogHandler(taxonomy.CatalogOptions{})
	if err != nil {
		w.t.Fatal(err)
	}
	cfg := contentkit.RuntimeConfig{
		EmbeddedConfig: contentkit.EmbeddedConfig{PG: w.pool, PGSchema: w.schema, Tenant: tenant},
		Auth:           w.ak.Authenticator(),
		UserInfo:       w.ak.UserInfo(),
		Content: content.Options{Resolver: w, ContentKinds: []string{"gallery"},
			Limits: content.Limits{Disabled: true}},
		Uploads: w.up, Reader: w.rd, ReadLimit: media.RateLimit{Disabled: true},
		Codes: router, Taxonomy: w.tax, Catalog: catalog,
	}
	if edit != nil {
		edit(&cfg)
	}
	if rt, err = contentkit.NewRuntime(w.t.Context(), cfg); err != nil {
		w.t.Fatal(err)
	}
	return rt
}

// person is a signed-in account holding role in the root group (zero: none)
// and its access token.
func (w *world) person(role iam.Role) (authtest.User, string) {
	w.t.Helper()
	u := authtest.NewUser(w.t, w.ak)
	if role != (iam.Role{}) {
		authtest.GrantRole(w.t, w.ak, iam.RootGroup(), iam.UserSubject(u.ID), role)
	}
	return u, authtest.SignIn(w.t, w.ak, u).AccessToken
}

// Resolve is the host's resolver: the gallery cid(1), owned by w.owner, is
// visible to everyone and edited by any signed-in person; an account's item
// is its person's and staff's to edit.
func (w *world) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		switch {
		case r.ContentKind == "gallery" && r.ContentID == cid(1):
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true, Owner: w.owner, Editor: a.Person()}
		case r.ContentKind == "user":
			editor, err := w.ownOrStaff(ctx, a, r.ContentID)
			if err != nil {
				return nil, err
			}
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true, Owner: r.ContentID, Editor: editor}
		}
	}
	return out, nil
}

// CanUpload is the host's upload authorizer: a person's own avatar (their
// quota), staff anyone's (exempt), and the gallery for any signed-in person.
func (w *world) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind != "user" {
		return media.UploadGrant{Allowed: a.Person()}, nil
	}
	if a.Person() && a.ID == t.Ref.ContentID {
		return media.UploadGrant{Allowed: true, Owner: a.ID}, nil
	}
	ok, err := a.Can(ctx, w.scope, w.media.String())
	return media.UploadGrant{Allowed: ok, Exempt: ok}, err
}

func (w *world) ownOrStaff(ctx context.Context, a access.Actor, id string) (bool, error) {
	if a.Person() && a.ID == id {
		return true, nil
	}
	return a.Can(ctx, w.scope, w.media.String())
}

// server serves rt's one mount under prefix, every response held to the
// route catalog.
type server struct {
	t       *testing.T
	h       http.Handler
	mux     *http.ServeMux
	checker *contract.Checker
	mu      sync.Mutex
	broken  []string
}

func serve(t *testing.T, rt *contentkit.Runtime) *server {
	t.Helper()
	checker, err := contract.NewChecker(os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s := &server{t: t, checker: checker, mux: mux}
	mux.Handle(prefix+"/", http.StripPrefix(prefix, rt.Handler()))
	s.h = checker.Wrap(prefix, mux, func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.broken = append(s.broken, err.Error())
	})
	t.Cleanup(func() {
		for _, b := range s.broken {
			t.Errorf("contract: %s", b)
		}
	})
	return s
}

// call sends one request with bearer ("" for none).
func (s *server) call(bearer, method, target string, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			s.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, prefix+target, &buf).WithContext(s.t.Context())
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// want calls and requires status, decoding a JSON body into out when given.
func (s *server) want(status int, bearer, method, target string, body, out any) *httptest.ResponseRecorder {
	s.t.Helper()
	rec := s.call(bearer, method, target, body)
	if rec.Code != status {
		s.t.Fatalf("%s %s: %d, want %d: %s", method, target, rec.Code, status, rec.Body)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatalf("%s %s: %v", method, target, err)
		}
	}
	return rec
}

// cid is the nth deterministic content id: a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func ptr[T any](v T) *T { return &v }

func pngBytes(t *testing.T, w int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	for i := range w {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// mount serves h at module path under the one mount, beside the runtime.
func (s *server) mount(path string, h http.Handler) {
	s.mux.Handle(prefix+path+"/", http.StripPrefix(prefix+path, h))
}
