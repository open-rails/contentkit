package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/workqueue"
)

// prefix is where the host mounts ContentKit's routes.
const prefix = "/api/contentkit"

// world is a host: AuthKit with root roles holding ContentKit's permissions,
// the media bucket, and the host's resolver and upload hooks for the
// galleries and accounts it still owns. Fixtures stand in for the media
// worker through their own registry over the same bucket and journal.
type world struct {
	t      *testing.T
	ak     *authkit.Client
	scope  auth.Scope // the root group: AuthKit's root scope, where staff hold their permissions
	media  iam.Perm   // edit any item's media
	staff  iam.Role
	pool   *pgxpool.Pool
	schema string
	tenant string // the media namespace too
	env    *mediatest.Env
	reg    *media.Registry
	jobs   *media.Jobs
	queue  *workqueue.Queue
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
	w.env = mediatest.Open(t)
	w.tenant = w.env.Tenant
	w.schema = "ck_e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range []string{w.schema, w.schema + "_river", w.schema + "_media_worker"} {
			_, _ = w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s}.Sanitize()+" CASCADE")
		}
		_, _ = w.pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = $1", w.schema)
	})
	return w
}

// mediaConfig is the site's media registry: galleries, accounts' avatars and
// ContentKit's own post and poll images.
func (w *world) mediaConfig() media.Config {
	images := []string{"image/png"}
	folder := func(name string) media.Kind {
		return media.Kind{Name: name, Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 10}},
			Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}}
	}
	return media.Config{BaseURL: "https://media.test",
		Kinds: []media.Kind{
			{Name: "gallery", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "originals/{name}", Types: images, MaxBytes: 1 << 20, Max: 10}, {Path: "cover", Types: images, MaxBytes: 1 << 20},
					{Path: "extras/{name}", Types: images, MaxBytes: 1 << 20, Max: 1}},
				Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{200}, Image: media.Image{MinWidth: 8}}}},
			{Name: "user", Namespace: "accounts", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "avatar", Types: images, MaxBytes: 1 << 20}},
				Public: []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{64, 128},
					Image: media.Image{Aspect: media.Square}, Default: "avatar.png"}}},
			folder("post"), folder("poll"),
		},
		Avatars: media.Avatars{Kind: "user", Preset: "avatar"}}
}

// config is the host's ContentKit configuration.
func (w *world) config() contentkit.Config {
	one := kinds.VersionRules{Languages: "one"}
	return contentkit.Config{Tenant: w.tenant, Database: contentkit.DatabaseConfig{Schema: w.schema},
		Content: kinds.Config{Languages: []string{"en"},
			Kinds: map[string]kinds.Kind{
				"gallery": {Route: "g", Presentation: "reader", Versions: one, Groups: []string{"tag", "series"}},
				"post":    {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
			},
			GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags", VersionLevel: true, Roles: []string{"featured"}}, "series": {Route: "series", Ordered: true}}},
		Interactions: contentkit.InteractionsConfig{Limits: contentkit.LimitsConfig{Disabled: true}},
		Media: &contentkit.MediaConfig{Config: w.mediaConfig(),
			Delivery:    contentkit.DeliveryConfig{SigningKey: "k1:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
			CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true}},
	}
}

// deps are the host's connections and providers: AuthKit passed straight in,
// and the hooks over the galleries and accounts it still owns.
func (w *world) deps() contentkit.Deps {
	return contentkit.Deps{Postgres: w.pool, Auth: w.ak.Authenticator(), Users: w.ak.UserInfo(), Store: w.env.Store,
		Resolver: w, CanUpload: w}
}

// galleries writes gallery contents cid(n), each with the version
// cid(n*10), as ContentKit's content routes will.
func (w *world) galleries(ns ...int) {
	w.t.Helper()
	q := pgx.Identifier{w.schema}.Sanitize()
	for _, n := range ns {
		if _, err := w.pool.Exec(w.t.Context(), `INSERT INTO `+q+`.content_items (tenant_id, id, kind) VALUES ($1, $2, 'gallery')`, w.tenant, cid(n)); err != nil {
			w.t.Fatal(err)
		}
		if _, err := w.pool.Exec(w.t.Context(), `INSERT INTO `+q+`.content_versions (tenant_id, id, content_id, languages) VALUES ($1, $2, $3, '{en}')`, w.tenant, cid(n*10), cid(n)); err != nil {
			w.t.Fatal(err)
		}
	}
}

// engine is the host's one ContentKit, started on its own River; edit
// adjusts its configuration first.
func (w *world) engine(edit func(*contentkit.Config, *contentkit.Deps)) *contentkit.Engine {
	w.t.Helper()
	cfg, deps := w.config(), w.deps()
	if edit != nil {
		edit(&cfg, &deps)
	}
	e, err := contentkit.New(w.t.Context(), cfg, deps)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = e.Close(context.Background()) })
	if err := e.Start(w.t.Context()); err != nil {
		w.t.Fatal(err)
	}
	// The fixtures' own view of the registry, as the media worker has one.
	mc := cfg.Media.Config
	mc.Namespace, mc.Hooks = w.tenant, media.Hooks{Resolver: w, CanUpload: w}
	if w.reg, err = media.NewRegistry(mc); err != nil {
		w.t.Fatal(err)
	}
	if w.queue, err = workqueue.New(w.pool, w.reg, w.schema+"_media_worker"); err != nil {
		w.t.Fatal(err)
	}
	journal, err := media.NewPGJournal(w.pool, w.schema, w.queue)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.jobs, err = media.NewJobs(media.JobsConfig{Store: w.env.Store, Registry: w.reg, Locker: media.PGLocker(w.pool), Journal: journal,
		Processes: w.queue, Pool: w.pool}); err != nil {
		w.t.Fatal(err)
	}
	return e
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

// server serves the engine's routes under prefix, every response held to
// the route catalog.
type server struct {
	t       *testing.T
	h       http.Handler
	checker *contract.Checker
	mu      sync.Mutex
	broken  []string
}

func serve(t *testing.T, e *contentkit.Engine) *server {
	t.Helper()
	checker, err := contract.NewChecker(os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, checker: checker}
	routes, err := e.Routes(contentkit.Routes{Prefix: prefix, Groups: contentkit.AllRoutes})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(strings.TrimSpace(r.Method+" "+r.Path), r.Handler)
	}
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

// call sends one request with bearer ("" for none) to target under the
// versioned prefix.
func (s *server) call(bearer, method, target string, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			s.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, prefix+"/v1"+target, &buf).WithContext(s.t.Context())
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
