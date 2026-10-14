package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
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
func TestAvatarsLiveInThePrimary(t *testing.T) {
	ctx := t.Context()
	doujins := newWorld(t)
	primary := doujins.engine(nil)
	api := httptest.NewServer(serve(t, primary).h)
	t.Cleanup(api.Close)
	key, err := doujins.ak.CreateAPIKey(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "hentai0", Role: doujins.avatars})
	if err != nil {
		t.Fatal(err)
	}

	hentai0 := newWorld(t)
	const mediaHost = "media.hentai0.test"
	engine := hentai0.engine(func(c *contentkit.Config, _ *contentkit.Deps) {
		c.Media.Config.BaseURL = "https://" + mediaHost
		c.Avatars = &contentkit.AvatarsConfig{Kind: avatarKind(), Primary: api.URL + prefix, Key: key.Secret}
	})
	site := serve(t, engine)
	alice, aliceToken := hentai0.person(iam.Role{})
	_, bobToken := hentai0.person(iam.Role{})

	var model struct {
		Avatars *media.PresetRule `json:"avatars"`
	}
	site.want(http.StatusOK, "", "GET", "/kinds", nil, &model)
	if a := model.Avatars; a == nil || a.Base != "https://"+mediaHost || a.Namespace != "accounts" || a.Kind != "user" || a.To != "avatar-{w}.webp" || !a.Stable {
		t.Fatalf("the content model's avatars %+v", model.Avatars)
	}

	body := pngBytes(t, 32)
	sum := sha256.Sum256(body)
	presign := media.PresignBody{Ref: media.RefBody{Kind: "user", ID: alice.ID}, Path: "avatar", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	site.want(http.StatusForbidden, bobToken, "POST", "/media/upload/presign", presign, nil)
	var plan media.PresignReply
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
	ref, err := doujins.reg.Ref("user", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, doujins, ref, plan.Path, "avatar") // the primary's worker

	var read media.ReadResult
	site.want(http.StatusOK, aliceToken, "GET", "/media/user/"+alice.ID+"?editor", nil, &read)
	if len(read.Files) != 1 || len(read.Public) != 1 || !strings.HasPrefix(read.Public[0].Renditions[0].URL, "https://"+mediaHost+"/v1/accounts/user/"+alice.ID+"/public/") {
		t.Fatalf("alice's editor read through the site %+v", read)
	}
	site.want(http.StatusForbidden, bobToken, "GET", "/media/user/"+alice.ID+"?editor", nil, nil)

	_, staff := hentai0.person(hentai0.staff)
	g, _ := hentai0.published(site, staff, "gallery")
	site.want(http.StatusCreated, aliceToken, "POST", "/contents/"+g+"/comments", content.CommentInput{Body: "hi"}, nil)
	var list []content.Comment
	site.want(http.StatusOK, "", "GET", "/contents/"+g+"/comments", nil, &list)
	stable := "https://" + mediaHost + "/v1/accounts/user/" + alice.ID + "/public/avatar-64.webp"
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
	gw, err := gateway.New(gateway.Config{Endpoint: s3.Endpoint, Region: s3.Region, AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey, Ring: ring,
		Hosts:   map[string][]string{mediaHost: {hentai0.tenant, "accounts"}},
		Buckets: map[string]string{hentai0.tenant: hentai0.env.Config.Bucket, "accounts": s3.Bucket}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, stable, nil).WithContext(ctx))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("the site's media host serves %d, %d bytes; want her %d", rec.Code, rec.Body.Len(), len(body))
	}

	for _, table := range []string{"content_media_commits", "content_media_allocations", "content_media_publications"} {
		var n int
		if err := hentai0.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{hentai0.schema, table}.Sanitize()+` WHERE tenant_id = 'accounts'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the site's %s holds %d avatar rows (%v)", table, n, err)
		}
	}
	var jobs int
	if err := hentai0.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{hentai0.schema + "_media_worker", "river_job"}.Sanitize()+`
WHERE args->'ref'->>'tenant_id' = 'accounts'`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("the site's media worker got %d avatar jobs (%v)", jobs, err)
	}
	for obj, err := range hentai0.env.Store.List(ctx, "accounts/") {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("the site's bucket holds %s", obj.Key)
	}

	if report, err := engine.EraseSubjects(ctx, []signal.Subject{{UserID: alice.ID}}); err != nil || !report.Complete() {
		t.Fatalf("erasing alice on the site: %+v %v", report, err)
	}
	if got := stableAvatar(t, doujins.env.Store, alice.ID); !bytes.Equal(got, body) {
		t.Fatal("the site's erasure reached the primary's avatar")
	}
	if report, err := primary.EraseSubjects(ctx, []signal.Subject{{UserID: alice.ID}}); err != nil || !report.Complete() {
		t.Fatalf("erasing alice on the primary: %+v %v", report, err)
	}
	for deadline := time.Now().Add(time.Minute); stableAvatar(t, doujins.env.Store, alice.ID) != nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the primary's erasure left alice's avatar")
		}
	}
}
