package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	mediaimage "github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/layout"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/signal"
)

// Two sites share their people (contentkit#131). The avatar primary holds
// every avatar; the other keeps none. A person signed in to the other
// site through its own AuthKit uploads their avatar through its routes,
// which forward to the primary's with the primary's key; nobody else may
// upload it. Their comments there show it at a URL built from their id on
// the site's own media host, which serves the image from the primary's
// bucket. Erasing them on the site leaves the avatar to the primary's
// erasure.
func TestAvatarsLiveInThePrimary(t *testing.T)           { testAvatars(t, false) }
func TestScopedAvatarsShareTenantOwnership(t *testing.T) { testAvatars(t, true) }
func testAvatars(t *testing.T, scoped bool) {
	ctx := t.Context()
	doujins := newWorld(t)
	accountSchema := doujins.schema + "_accounts"
	t.Cleanup(func() {
		_, _ = doujins.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{accountSchema}.Sanitize()+" CASCADE")
	})
	tenantID := contentkit.NewTenantID()
	var alice authtest.User
	var aliceToken string
	var accounts *authkit.Client
	if scoped {
		// Scoped apps share one actual native tenant directory, not merely
		// matching subject strings across unrelated account stores.
		alice, aliceToken = doujins.person(iam.Role{})
		var err error
		tenantID, err = contentkit.ParseTenantID(alice.TenantID.String())
		if err != nil {
			t.Fatal(err)
		}
		accounts, err = doujins.ak.ForTenant(ctx, alice.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		// accounts borrows doujins.ak; only that original owner closes it.
	}
	var replicaConfig contentkit.Config
	var replicaDeps contentkit.Deps
	primary := doujins.engine(func(c *contentkit.Config, deps *contentkit.Deps) {
		c.Accounts = &contentkit.AccountsConfig{Namespace: "accounts", Schema: accountSchema, Avatars: c.Avatars}
		c.Avatars = nil
		if scoped {
			c.Scope = &contentkit.Scope{TenantID: tenantID, AppID: contentkit.NewAppID()}
			c.LegacyCatalogKey, c.Tenant = c.Tenant, ""
			c.Accounts.Namespace = ""
			deps.Authorization = avatarAuthorization{doujins.scope}
			deps.Users = accounts.UserInfo()
			replicaConfig, replicaDeps = *c, *deps
		}
	})
	if scoped {
		replicaConfig.Database.SkipMigrations = true
		replica, err := contentkit.New(ctx, replicaConfig, replicaDeps)
		if err != nil {
			t.Fatalf("same primary app replica failed: %v", err)
		}
		if err = replica.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	primarySite := serve(t, primary)
	api := httptest.NewServer(primarySite.h)
	t.Cleanup(api.Close)
	key, err := doujins.ak.CreateAPIKey(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "hentai0", Role: doujins.avatars})
	if err != nil {
		t.Fatal(err)
	}

	hentai0 := newWorld(t)
	if scoped {
		// Both engines borrow the same fixed-tenant native directory. The
		// unused secondary fixture owner retains its own registered cleanup.
		hentai0.ak, hentai0.scope = accounts, doujins.scope
	}
	const mediaHost = "media.hentai0.test"
	engine := hentai0.engine(func(c *contentkit.Config, deps *contentkit.Deps) {
		c.Media.Config.BaseURL = "https://" + mediaHost
		c.Accounts = &contentkit.AccountsConfig{Namespace: "accounts", Schema: accountSchema,
			Avatars: &contentkit.AvatarsConfig{Kind: avatarKind(), Primary: api.URL + prefix, Key: key.Secret}}
		c.Avatars = nil
		if scoped {
			c.Scope = &contentkit.Scope{TenantID: tenantID, AppID: contentkit.NewAppID()}
			c.LegacyCatalogKey, c.Tenant = c.Tenant, ""
			c.Accounts.Namespace = ""
			deps.Authorization = avatarAuthorization{hentai0.scope}
		}
	})
	site := serve(t, engine)
	if !scoped {
		alice, aliceToken = hentai0.person(iam.Role{})
	}
	_, bobToken := hentai0.person(iam.Role{})

	ns, root := "accounts", "accounts/"
	if scoped {
		scope := layout.Scope{TenantID: tenantID.String()}
		ns, root = scope.Partition(), scope.Root()
	}
	var model struct {
		Avatars *media.PresetRule `json:"avatars"`
	}
	site.want(http.StatusOK, "", "GET", "/kinds", nil, &model)
	if a := model.Avatars; a == nil || a.Base != "https://"+mediaHost || a.Namespace != ns || a.Kind != "user" || a.To != "avatar-{w}.webp" || !a.Stable {
		t.Fatalf("the content model's avatars %+v", model.Avatars)
	}

	body := pngBytes(t, 32)
	sum := sha256.Sum256(body)
	presign := media.PresignBody{Ref: media.RefBody{Kind: "user", ID: alice.ID}, Path: "avatar", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	site.want(http.StatusForbidden, bobToken, "POST", "/media/upload/presign", presign, nil)
	if scoped {
		unknown := presign
		unknown.Ref.ID = contentref.NewID()
		primarySite.want(http.StatusForbidden, key.Secret, "POST", "/media/upload/presign", unknown, nil)
	}
	var plan media.PresignReply
	upload := func() {
		site.want(http.StatusOK, aliceToken, "POST", "/media/upload/presign", presign, &plan)
		req, _ := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT: %d %s", resp.StatusCode, b)
		}
		site.want(http.StatusOK, aliceToken, "POST", "/media/upload/commit", media.CommitBody{Ref: presign.Ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	}
	upload()

	ref, err := doujins.reg.Ref("user", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	process := func() {
		if !scoped {
			publish(t, doujins, ref, plan.Path, "avatar")
			return
		}
		manifests := doujins.jobs.Manifests()
		if _, err := manifests.Place(ctx, ref); err != nil {
			t.Fatal(err)
		}
		processor, err := mediaimage.New(mediaimage.Config{Store: doujins.env.Store, Manifests: manifests})
		if err != nil {
			t.Fatal(err)
		}
		if err = processor.Process(ctx, media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	process()
	if scoped {
		crop := media.Crop{X: 2, Y: 2, W: 24, H: 24}
		site.want(http.StatusOK, aliceToken, "POST", "/media/upload/commit", media.CommitBody{Ref: presign.Ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpEdit, Path: plan.Path, Edit: &media.Edit{Crop: &crop}}}}, nil)
		process()
		var edited media.ReadResult
		site.want(http.StatusOK, aliceToken, "GET", "/media/user/"+alice.ID+"?editor", nil, &edited)
		if len(edited.Files) != 1 || edited.Files[0].Edit == nil || edited.Files[0].Edit.Crop == nil || edited.Files[0].Edit.Crop.X != crop.X {
			t.Fatalf("shared crop was not retained: %+v", edited.Files)
		}
	}

	rendered := avatarBytes(t, doujins.env.Store, root+"user/"+alice.ID+"/public/avatar-64.webp")
	var read media.ReadResult
	site.want(http.StatusOK, aliceToken, "GET", "/media/user/"+alice.ID+"?editor", nil, &read)
	if len(read.Files) != 1 || len(read.Public) != 1 || !strings.HasPrefix(read.Public[0].Renditions[0].URL, "https://"+mediaHost+"/v1/"+root+"user/"+alice.ID+"/public/") {
		t.Fatalf("alice's editor read through the site %+v", read)
	}
	site.want(http.StatusForbidden, bobToken, "GET", "/media/user/"+alice.ID+"?editor", nil, nil)

	_, staff := hentai0.person(hentai0.staff)
	g, _ := hentai0.published(site, staff, "gallery")
	site.want(http.StatusCreated, aliceToken, "POST", "/contents/"+g+"/comments", content.CommentInput{Body: "hi"}, nil)
	var list []content.Comment
	site.want(http.StatusOK, "", "GET", "/contents/"+g+"/comments", nil, &list)
	stable := "https://" + mediaHost + "/v1/" + root + "user/" + alice.ID + "/public/avatar-64.webp"
	if len(list) != 1 || list[0].Author == nil || list[0].Author.Avatar != stable {
		t.Fatalf("alice's comment on the site %+v", list)
	}
	signing, err := token.ParseKey("k1:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := token.NewRing(signing, nil)
	if err != nil {
		t.Fatal(err)
	}
	s3 := doujins.env.Config
	gc := gateway.Config{Endpoint: s3.Endpoint, Region: s3.Region, AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey, Ring: ring,
		Hosts:   map[string][]string{mediaHost: {hentai0.tenant, "accounts"}},
		Buckets: map[string]string{hentai0.tenant: hentai0.env.Config.Bucket, "accounts": s3.Bucket}}
	if scoped {
		gc.Routes = []gateway.Route{{Host: mediaHost, Scope: layout.Scope{TenantID: tenantID.String()}, Bucket: s3.Bucket}}
	}
	gw, err := gateway.New(gc)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stable, nil).WithContext(ctx))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), rendered) {
		t.Fatalf("the site's media host serves %d, %d bytes; want her %d", rec.Code, rec.Body.Len(), len(body))
	}

	for _, table := range []string{"content_media_commits", "content_media_allocations", "content_media_publications"} {
		var n int
		if err := hentai0.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{hentai0.schema, table}.Sanitize()+` WHERE tenant_id = $1`, ns).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the site's %s holds %d avatar rows (%v)", table, n, err)
		}
	}
	var jobs int
	if err := hentai0.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{hentai0.schema + "_media_worker", "river_job"}.Sanitize()+`
WHERE args->'ref'->>'tenant_id' = $1`, ns).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("the site's media worker got %d avatar jobs (%v)", jobs, err)
	}
	for obj, err := range hentai0.env.Store.List(ctx, root) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("the site's bucket holds %s", obj.Key)
	}

	beforeAvatar, _, err := doujins.jobs.Manifests().Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(beforeAvatar)
	erase := engine.EraseSubjects
	if scoped {
		erase = engine.EraseAppSubjects
	}
	if report, err := erase(ctx, []signal.Subject{{UserID: alice.ID}}); err != nil || !report.Complete() {
		t.Fatalf("erasing alice on the site: %+v %v", report, err)
	}
	if scoped {
		if report, err := engine.ReplayErasureFences(ctx); err != nil || !report.Complete() {
			t.Fatalf("replay app fences: %+v %v", report, err)
		}
		after, _, err := doujins.jobs.Manifests().Get(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		afterJSON, _ := json.Marshal(after)
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatal("app erasure/replay changed shared avatar manifest")
		}
	}
	if got := avatarBytes(t, doujins.env.Store, root+"user/"+alice.ID+"/public/avatar-64.webp"); !bytes.Equal(got, rendered) {
		t.Fatal("the site's erasure reached the primary's avatar")
	}
	if scoped {
		site.want(http.StatusOK, aliceToken, "POST", "/media/upload/commit", media.CommitBody{Ref: presign.Ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRemove, Path: plan.Path}}}, nil)
		if got := avatarBytes(t, doujins.env.Store, root+"user/"+alice.ID+"/public/avatar-64.webp"); got != nil {
			t.Fatal("shared avatar removal left its public image")
		}
		upload()
		process()
	}
	if scoped {
		failing := &fenceFailureStore{Store: replicaDeps.Store, fail: true}
		repairDeps := replicaDeps
		repairDeps.Store = failing
		recovery, err := contentkit.NewForErasureReplay(ctx, replicaConfig, repairDeps)
		if err != nil {
			t.Fatal(err)
		}
		defer recovery.Close(ctx)
		if report, err := recovery.EraseSubjects(ctx, []signal.Subject{{UserID: alice.ID}}); err == nil || report.Complete() {
			t.Fatalf("failed fence reported completion: %+v %v", report, err)
		}
		if report, err := recovery.ReplayErasureFences(ctx); err == nil || report.Complete() {
			t.Fatalf("app replay discharged failed avatar cleanup: %+v %v", report, err)
		}
		if reopened, err := contentkit.New(ctx, replicaConfig, replicaDeps); !errors.Is(err, contentkit.ErrErasureReplayRequired) {
			if reopened != nil {
				_ = reopened.Close(ctx)
			}
			t.Fatalf("pending global cleanup allowed startup: %v", err)
		}
		failing.fail = false
		if report, err := recovery.ReplayAccountErasures(ctx); err != nil || !report.Complete() {
			t.Fatalf("full recovery: %+v %v", report, err)
		}
		if report, err := recovery.ReplayErasureFences(ctx); err != nil || !report.Complete() {
			t.Fatalf("local recovery: %+v %v", report, err)
		}
		reopened, err := contentkit.New(ctx, replicaConfig, replicaDeps)
		if err != nil {
			t.Fatalf("completed recovery cannot reopen: %v", err)
		}
		_ = reopened.Close(ctx)
	} else if report, err := primary.EraseSubjects(ctx, []signal.Subject{{UserID: alice.ID}}); err != nil || !report.Complete() {
		t.Fatalf("erasing alice on primary: %+v %v", report, err)
	}

	for deadline := time.Now().Add(time.Minute); avatarBytes(t, doujins.env.Store, root+"user/"+alice.ID+"/public/avatar-64.webp") != nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the primary's erasure left alice's avatar")
		}
	}
}

// Explicit fixture membership: authentication remains real AuthKit and the
// fixture maps its verified subject into this test tenant's account directory.
type avatarAuthorization struct{ root auth.Scope }

func (a avatarAuthorization) ResolveActor(_ context.Context, _ contentkit.Scope, v auth.Verified) (string, error) {
	return v.Identity().Subject, nil
}
func (a avatarAuthorization) Authorize(ctx context.Context, _ contentkit.Scope, v auth.Verified, t contentkit.PermissionTarget) (bool, error) {
	checker, ok := v.(auth.PermissionChecker)
	if !ok {
		return false, auth.ErrUnavailable
	}
	return checker.Can(ctx, a.root, t.Permission)
}
func avatarBytes(t *testing.T, s media.Store, key string) []byte {
	t.Helper()
	r, _, e := s.Get(t.Context(), key, media.GetOptions{})
	if errors.Is(e, media.ErrNotFound) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	b, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

type fenceFailureStore struct {
	media.Store
	fail bool
}

func (s *fenceFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if s.fail && strings.HasSuffix(key, "/public/"+layout.ErasureFenceName) {
		return media.Object{}, errors.New("injected erasure fence outage")
	}
	return s.Store.Put(ctx, key, body, size, o)
}
