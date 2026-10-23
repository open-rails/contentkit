package contentkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

type audienceReleaseDownloadPolicy struct{ seen []versions.DownloadRequest }

func (p *audienceReleaseDownloadPolicy) Decide(_ context.Context, r versions.DownloadRequest) (versions.DownloadDecision, error) {
	p.seen = append(p.seen, r)
	return versions.DownloadDecision{Allowed: true}, nil
}

func (*audienceReleaseDownloadPolicy) Mark(_ context.Context, _ access.Actor, _ versions.Target, _ int, b []byte) ([]byte, string, error) {
	return b, ".webp", nil
}

// Time gates authorize already prepared private media without running any
// River worker. In particular, a stale hidden/public-copy projection must not
// delay a release or grant a URL before both publication dates have arrived.
func TestEngineAudienceReleaseMediaWithoutRecorderIntegration(t *testing.T) {
	for _, kind := range []string{"blog", "gallery"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			env := mediatest.Open(t)
			pool := pgtest.Pool(t, nil)
			schema := engineSchema(t, pool)
			key := token.Key{ID: "release", Secret: bytes.Repeat([]byte("r"), 32)}
			ring, err := token.NewRing(key, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg := siteConfig(schema)
			cfg.Tenant = env.Tenant
			cfg.Content.Tiers, cfg.Content.TokenClaims = []string{"early", "later"}, []string{"early", "later"}
			mk := media.Kind{Name: kind,
				Uploads: []media.Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
				Private: []media.Private{{Name: "page", From: "originals/{name}", To: "pages/{name}.webp", Image: &media.Image{Width: 800}}},
				Public:  []media.Public{{Name: "cover", From: "originals/{name}", To: "cover-{n}-{w}.webp", Widths: []int{64}, First: 1}},
			}
			if kind == "gallery" {
				mk.Publish = &media.Publish{Pages: "originals/{name}", Renditions: []string{"page"}, Download: "archive"}
				mk.Private = append(mk.Private,
					media.Private{Name: "archive-page", From: "originals/{name}", To: "archive-pages/{name}.webp", Image: &media.Image{}},
					media.Private{Name: "archive", To: "download/pages.zip", Download: "{title}.zip", Zip: "archive-pages/", HostOnly: true})
			}
			cfg.Media = &MediaConfig{Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{mk}},
				Delivery:  DeliveryConfig{Mode: "url", SigningKey: key.ID + ":" + base64.RawURLEncoding.EncodeToString(key.Secret)},
				ReadLimit: RateLimitConfig{Disabled: true}}
			auth := testauth.Auth{Authz: staffAuthz{}, Claims: map[string][]string{"early": {"early"}, "later": {"later"}}}
			policy := &audienceReleaseDownloadPolicy{}
			e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: auth, Store: env.Store, Downloads: policy})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close(ctx) })
			fleet, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema + "_river"}, e.RiverJobs())
			if err != nil {
				t.Fatal(err)
			}
			// Bind producers so writes queue their work, but never start the fleet.
			if err := e.Start(ctx, WithRiverClient(fleet)); err != nil {
				t.Fatal(err)
			}
			routes, err := e.Routes(Routes{Prefix: "/ck", Groups: ContentRoutes | MediaRoutes})
			if err != nil {
				t.Fatal(err)
			}
			s := serve(t, routes)
			staff := access.Actor{ID: "staff", Kind: "user"}
			early := access.Actor{ID: "early", Kind: "user", IP: "192.0.2.1"}
			later := access.Actor{ID: "later", Kind: "user", IP: "192.0.2.2"}
			anon := access.Actor{Anonymous: true, IP: "192.0.2.3"}
			start := time.Now().UTC().Truncate(time.Millisecond)
			contentAt := start.Add(time.Hour)
			versionAt := contentAt.Add(time.Hour)
			if kind == "blog" {
				versionAt = contentAt // a hidden version inherits its content's release
			}
			laterAt, publicAt := versionAt.Add(time.Hour), versionAt.Add(2*time.Hour)
			rule := access.Rule{Level: access.PPV, Releases: []access.Release{
				{At: contentAt.Add(-time.Minute), Level: access.TierLevel, Tiers: []string{"early"}},
				{At: laterAt, Level: access.TierLevel, Tiers: []string{"later"}},
				{At: publicAt, Level: access.Public},
			}}
			input := catalog.ContentInput{Kind: kind, Access: &rule, Text: map[string]catalog.Text{"en": {Title: "Timed release"}}}
			if kind == "gallery" {
				input.Versions = []catalog.VersionInput{{Languages: []string{"en"}, State: catalog.StatePublished, PublishAt: &versionAt}}
			}
			var c catalog.Content
			s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", input, &c)
			vid := c.Versions[0].ID
			ref, err := e.media.reg.VersionRef(kind, c.ID, vid)
			if err != nil {
				t.Fatal(err)
			}
			item, err := e.media.reg.Item(ref)
			if err != nil {
				t.Fatal(err)
			}
			body := pngBytes(t, 16)
			sum := sha256.Sum256(body)
			allocations, err := e.media.jobs.Manifests().NewBlobs(ctx, ref, layout.AreaPrivate, [][]byte{sum[:]})
			if err != nil {
				t.Fatal(err)
			}
			blob := allocations[0]
			object, err := item.Blob(blob)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store.Put(ctx, object, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/webp"}); err != nil {
				t.Fatal(err)
			}
			// Stand in for encoding only: use the real manifest, snapshot storage,
			// catalog/gate, media signer, and object gateway for every read.
			upload := media.File{Path: "originals/page", Blob: blob, Type: "image/png", Size: int64(len(body)), W: 16, H: 16}
			output := media.File{Path: "pages/page.webp", Blob: blob, Type: "image/webp", Size: int64(len(body)), W: 16, H: 16,
				From: upload.Path, Preset: "page"}
			files := []media.File{upload, output}
			if kind == "gallery" {
				allocations, err := e.media.jobs.Manifests().NewBlobs(ctx, ref, layout.AreaDownload, [][]byte{sum[:]})
				if err != nil {
					t.Fatal(err)
				}
				upload.Blob = allocations[0]
				downloadKey, err := item.Blob(upload.Blob)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := env.Store.Put(ctx, downloadKey, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/webp"}); err != nil {
					t.Fatal(err)
				}
				files = []media.File{upload, output, {Path: "archive-pages/page.webp", Blob: upload.Blob, Type: "image/webp", Size: int64(len(body)),
					W: 16, H: 16, From: upload.Path, Preset: "archive-page"}}
			}
			if _, err := e.media.jobs.Manifests().Edit(ctx, ref, func(m *media.Manifest) error {
				m.Hidden, m.Files = true, files
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if kind == "gallery" {
				snapshot, err := json.Marshal(map[string]any{"pages": []any{map[string]any{
					"name": "page", "path": upload.Path, "source": upload.Key(), "files": []any{map[string]any{
						"preset": output.Preset, "path": output.Path, "blob": output.Blob, "type": output.Type,
						"size": output.Size, "w": output.W, "h": output.H,
					}},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_version_publications"}.Sanitize()+`
(tenant_id,id,version_id,kind,state,source) VALUES ($1,$2,$3,'pages','current',$4)`, env.Tenant, uuid.NewString(), vid, snapshot); err != nil {
					t.Fatal(err)
				}
			}
			s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+c.ID+"/publish", catalog.PublishInput{PublishAt: &contentAt}, nil)
			mediaURL := "/ck/v1/media/" + kind + "/" + c.ID + "/versions/" + vid
			gw, err := gateway.New(gateway.Config{Endpoint: env.Config.Endpoint, Region: env.Config.Region,
				AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Ring: ring,
				Hosts: map[string][]string{"media.test": {env.Tenant}}, Buckets: map[string]string{env.Tenant: env.Config.Bucket}})
			if err != nil {
				t.Fatal(err)
			}
			fetch := func(rawURL string, want int) {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, rawURL, nil).WithContext(ctx)
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				if rec.Code != want || want == http.StatusOK && !bytes.Equal(rec.Body.Bytes(), body) {
					t.Fatalf("gateway %s: %d %q, want %d", rawURL, rec.Code, rec.Body.Bytes(), want)
				}
			}
			asOf := func(at time.Time) time.Time {
				t.Helper()
				// Shift only this fixture's stored timestamps, including its
				// facets, to exercise PostgreSQL's real clock without sleeping.
				// Leave published_at NULL as if the recorder has not run.
				delta := time.Now().UTC().Sub(at)
				shifted := rule
				shifted.Releases = slices.Clone(rule.Releases)
				for i := range shifted.Releases {
					shifted.Releases[i].At = shifted.Releases[i].At.Add(delta)
				}
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "content_items"}.Sanitize()+`
SET publish_at=$3,published_at=NULL,access=$4 WHERE tenant_id=$1 AND id=$2`, env.Tenant, c.ID, contentAt.Add(delta), shifted); err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "content_versions"}.Sanitize()+`
SET publish_at=$3,published_at=NULL WHERE tenant_id=$1 AND id=$2`, env.Tenant, vid, versionAt.Add(delta)); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "content_facets"}.Sanitize()+`
SET live_from=$3,published_at=$4,access_releases=$5 WHERE tenant_id=$1 AND version_id=$2`, env.Tenant, vid, versionAt.Add(delta), contentAt.Add(delta), shifted.Releases)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return versionAt.Add(delta).Truncate(time.Microsecond)
			}
			before := []time.Time{start}
			if kind == "gallery" {
				before = append(before, contentAt)
			}
			for _, at := range before {
				asOf(at)
				for _, who := range []access.Actor{early, later, anon} {
					s.call(http.StatusNotFound, who, "GET", mediaURL, nil, nil)
				}
			}
			for _, phase := range []struct {
				at      time.Time
				allowed []bool
			}{{versionAt, []bool{true, false, false}}, {laterAt, []bool{true, true, false}}, {publicAt, []bool{true, true, true}}} {
				releasedAt := asOf(phase.at)
				for i, who := range []access.Actor{early, later, anon} {
					var read media.ReadResult
					s.call(http.StatusOK, who, "GET", mediaURL, nil, &read)
					if len(read.Public) != 0 || read.Cookie != nil {
						t.Fatalf("stopped workers unexpectedly exposed public files or a cookie: %+v", read)
					}
					if !phase.allowed[i] {
						if read.Access != media.AccessNone || len(read.Files) != 0 {
							t.Fatalf("unreleased audience %q received media at %v: %+v", who.ID, phase.at, read)
						}
						continue
					}
					if len(read.Files) != 1 || read.Files[0].Path != output.Path || read.Files[0].URL == "" {
						t.Fatalf("released audience %q has no prepared media at %v: %+v", who.ID, phase.at, read)
					}
					fetch(read.Files[0].URL, http.StatusOK)
					unsigned, _ := url.Parse(read.Files[0].URL)
					unsigned.RawQuery = ""
					fetch(unsigned.String(), http.StatusNotFound)
					if kind == "gallery" {
						var plan versions.ReaderPlan
						s.call(http.StatusOK, who, "GET", "/ck/v1/contents/"+c.ID+"/versions/"+vid+"/reader", nil, &plan)
						if len(plan.Segments) != 1 || plan.Segments[0].Locked || plan.Segments[0].Item == nil {
							t.Fatalf("released audience has no reader plan: %+v", plan)
						}
						var download versions.FileDownload
						s.call(http.StatusOK, who, "GET", "/ck/v1/contents/"+c.ID+"/versions/"+vid+"/download", nil, &download)
						if len(policy.seen) == 0 || !policy.seen[len(policy.seen)-1].Released.Equal(releasedAt) {
							t.Fatalf("download policy lost the runtime release time: %+v", policy.seen)
						}
					}
				}
			}
			manifest, _, err := e.media.jobs.Manifests().Get(ctx, ref)
			if err != nil || !manifest.Hidden {
				t.Fatalf("reads changed the hidden projection: %+v, %v", manifest, err)
			}
			for object, err := range env.Store.List(ctx, item.PublicPrefix()) {
				t.Fatalf("reads wrote a public copy: %+v, %v", object, err)
			}
			var recorded *time.Time
			if err := pool.QueryRow(ctx, `SELECT published_at FROM `+pgx.Identifier{schema, "content_items"}.Sanitize()+`
WHERE tenant_id=$1 AND id=$2`, env.Tenant, c.ID).Scan(&recorded); err != nil || recorded != nil {
				t.Fatalf("publication depended on its recorder: %v, %v", recorded, err)
			}
			var completed int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema + "_river", "river_job"}.Sanitize()+`
WHERE state='completed'`).Scan(&completed); err != nil || completed != 0 {
				t.Fatalf("a worker ran: completed=%d, %v", completed, err)
			}
			// The retained bytes are available in the private store throughout;
			// the time-sensitive authorization lives at URL issuance.
			r, _, err := env.Store.Get(ctx, object, media.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			stored, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(stored, body) {
				t.Fatalf("private source changed: %q, %v", stored, err)
			}
		})
	}
}
