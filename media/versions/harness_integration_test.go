package versions_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

var (
	pngs   = []string{"image/png"}
	signer = token.Key{ID: "k", Secret: bytes.Repeat([]byte("k"), 32)}
)

// mediaKinds is a site's media: galleries publishing their versions' pages,
// previews, audio, archives and downloads, with a content-level cover;
// movies with an HLS ladder, subtitles and audio tracks per version; and
// people's avatars.
func mediaKinds(ns string, hooks media.Hooks) media.Config {
	return media.Config{Namespace: ns, BaseURL: "https://media.example", Hooks: hooks,
		Avatars: media.Avatars{Kind: "user", Preset: "avatar"},
		Kinds: []media.Kind{
			{Name: "gallery", Versions: true, KeepOriginals: true,
				Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"low", "thumb"}, Previews: "preview",
					Playback: "listen", Import: "import/{name}", Download: "zip"},
				Uploads: []media.Upload{
					{Path: "cover", Content: true, Types: pngs, MaxBytes: 10 << 20},
					{Path: "originals/{name}", Types: pngs, MaxBytes: 10 << 20},
					{Path: "audio/{name}", Types: []string{"audio/wav"}, MaxBytes: 10 << 20},
					{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 10 << 20},
					{Path: "previews/{name}", Types: []string{"image/webp"}, MaxBytes: 10 << 20, Max: 3},
				},
				Private: []media.Private{
					{Name: "low", From: "originals/{name}", To: "low-res/{name}.webp", Image: &media.Image{Width: 120, Height: 180}},
					{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 40, Height: 60, Fit: media.FitCover}},
					{Name: "high", From: "originals/{name}", To: "high/{name}.webp", Image: &media.Image{}},
					{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "high/", HostOnly: true},
					{Name: "listen", From: "audio/{name}", To: "listen/{name}/", Audio: &media.Audio{}},
				},
				Public: []media.Public{
					{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{64}},
					{Name: "preview", From: "previews/{name}", To: "preview-{n}.webp", First: 3},
				}},
			{Name: "movie", Versions: true, KeepOriginals: true,
				Uploads: []media.Upload{
					{Path: "cover", Content: true, Types: pngs, MaxBytes: 10 << 20},
					{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 30},
					{Path: "subs/{name}", Types: media.SubtitleTypes, MaxBytes: 1 << 20},
					{Path: "poster", Types: pngs, MaxBytes: 10 << 20, Frames: "source"},
				},
				Private: []media.Private{
					{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{Ladder: []int{1080, 480}}},
					{Name: "mp4", From: "source", To: "video/{name}-480.mp4", MP4: media.Rung(480)},
					{Name: "vtt", From: "subs/{name}", To: "captions/{name}.vtt", Subtitles: &media.Subtitles{}},
				},
				Public: []media.Public{
					{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{64}},
					{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{64}},
				}},
			{Name: "user", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "avatar", Types: pngs, MaxBytes: 1 << 20}},
				Public:  []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{32}}}},
		}}
}

// siteKinds is the site's one kinds list: galleries and movies, tags and
// user-owned channels.
var siteKinds = kinds.Config{Languages: []string{"en"},
	Kinds: map[string]kinds.Kind{
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Segments: true, Groups: []string{"tag", "channel"}},
		"movie":   {Route: "m", Presentation: "player", Versions: kinds.VersionRules{Languages: "many"}},
	},
	GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags"}, "channel": {Owner: "user", AuthScope: true}}}

// grants are the test auth's permissions: staff holds root:groups:write;
// in a channel's AuthKit scope, its editor holds channel:media:write.
type grants struct {
	staff  string
	scoped map[string]string // scope id -> editor id
}

func (g *grants) Can(context.Context, access.Actor, string) (bool, error) { return false, nil }

func (g *grants) CanIn(_ context.Context, a access.Actor, scope auth.Scope, perm string) (bool, error) {
	if scope == testauth.Scope {
		return a.ID == g.staff && perm == string(access.GroupsWrite), nil
	}
	return g.scoped[scope.ID] == a.ID && perm == "channel:media:write", nil
}

// catalog stands in for the catalog's and the gate's decisions over the
// contract tables (lanes 120-catalog and 121-access implement them): a
// version is live when it and its content are published, due and not
// deleted; staff and creators edit; a "tier" rule opens to holders of its
// tiers and its "free" leading segments to everyone.
type catalog struct {
	pool   *pgxpool.Pool
	schema string
	tenant string
	staff  string
	tiers  map[string][]string // actor id -> held tiers

	mu        sync.Mutex
	refreshed []string
}

func (c *catalog) t(name string) string { return pgx.Identifier{c.schema, name}.Sanitize() }

func (c *catalog) See(ctx context.Context, actor access.Actor, targets []versions.Target) (map[versions.Target]versions.Sight, error) {
	out := map[versions.Target]versions.Sight{}
	for _, t := range targets {
		var live, creator bool
		err := c.pool.QueryRow(ctx, `SELECT c.state = 'published' AND c.deleted_at IS NULL AND
	($3 = '' OR v.state = 'published' AND (v.publish_at IS NULL OR v.publish_at <= now()) AND v.deleted_at IS NULL),
	EXISTS (SELECT 1 FROM `+c.t("content_creators")+` cr WHERE cr.tenant_id = c.tenant_id AND cr.content_id = c.id AND cr.user_id = $4)
FROM `+c.t("content_items")+` c LEFT JOIN `+c.t("content_versions")+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id AND v.id = $3
WHERE c.tenant_id = $1 AND c.id = $2 AND ($3 = '' OR v.id IS NOT NULL)`, c.tenant, t.Content, t.Version, actor.ID).Scan(&live, &creator)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		editor := actor.Person() && (creator || actor.ID == c.staff)
		out[t] = versions.Sight{Visible: live || editor, Live: live, Editor: editor}
	}
	return out, nil
}

func (c *catalog) Open(ctx context.Context, actor access.Actor, vs []versions.Target) (map[versions.Target]versions.Opening, error) {
	out := map[versions.Target]versions.Opening{}
	for _, t := range vs {
		var rule struct {
			Level string   `json:"level"`
			Tiers []string `json:"tiers"`
			Free  int      `json:"free"`
		}
		var raw []byte
		if err := c.pool.QueryRow(ctx, `SELECT COALESCE(v.access, c.access) FROM `+c.t("content_versions")+` v
JOIN `+c.t("content_items")+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id WHERE v.tenant_id = $1 AND v.id = $2`,
			c.tenant, t.Version).Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			return nil, err
		}
		held, _ := c.Tiers(ctx, actor)
		opens := rule.Level == "public" || slices.ContainsFunc(rule.Tiers, func(t string) bool { return slices.Contains(held, t) })
		out[t] = versions.Opening{Opens: opens, Free: rule.Free}
	}
	return out, nil
}

func (c *catalog) Tiers(_ context.Context, actor access.Actor) ([]string, error) {
	if actor.Anonymous {
		return nil, nil
	}
	return c.tiers[actor.ID], nil
}

func (c *catalog) RefreshTx(_ context.Context, _ pgx.Tx, ids []string) error {
	c.mu.Lock()
	c.refreshed = append(c.refreshed, ids...)
	c.mu.Unlock()
	return nil
}

func (c *catalog) wasRefreshed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.refreshed, id)
}

// marker is a host watermark: it marks who's second page by appending
// their id.
type marker struct{ who string }

func (m *marker) Pages(_ context.Context, a access.Actor, _ versions.Target, _ int) ([]int, error) {
	if a.ID != m.who {
		return nil, nil
	}
	return []int{1}, nil
}

func (*marker) Mark(_ context.Context, a access.Actor, _ versions.Target, _ int, page []byte) ([]byte, string, error) {
	return append(slices.Clone(page), []byte("marked:"+a.ID)...), ".png", nil
}

// site is one ContentKit tenant: its catalog rows, media runtime, the media
// worker beside it and the HTTP API.
type site struct {
	*s3test.Env
	t       *testing.T
	ctx     context.Context
	tenant  string
	pool    *pgxpool.Pool
	schema  string
	cat     *catalog
	grants  *grants
	reg     *media.Registry
	ms      *media.Manifests
	uploads *media.Uploads
	jobs    *media.Jobs
	reader  *media.Reader
	svc     *versions.Service
	srv     *httptest.Server

	staff, creator, fan, stranger access.Actor
}

type siteOptions struct {
	watermark versions.Watermarker
	downloads versions.Downloads
}

func newSite(t *testing.T, o siteOptions) *site {
	t.Helper()
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	s := &site{Env: env, t: t, ctx: ctx, tenant: env.Tenant, pool: env.Pool(), schema: env.ContentSchema()}
	person := func() access.Actor { return access.Actor{ID: contentref.NewID(), Kind: "user"} }
	s.staff, s.creator, s.fan, s.stranger = person(), person(), person(), person()
	s.cat = &catalog{pool: s.pool, schema: s.schema, tenant: s.tenant, staff: s.staff.ID,
		tiers: map[string][]string{s.fan.ID: {"premium"}}}
	s.grants = &grants{staff: s.staff.ID, scoped: map[string]string{}}
	authn := testauth.Auth{Authz: s.grants}
	var err error
	s.svc, err = versions.New(versions.Options{Tenant: s.tenant, Schema: s.schema, Pool: s.pool, Kinds: siteKinds, Auth: authn,
		Visibility: s.cat, Access: s.cat, Facets: s.cat, Watermark: o.watermark, Downloads: o.downloads})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := media.ForContent(mediaKinds(s.tenant, s.svc.Hooks()), siteKinds)
	if err != nil {
		t.Fatal(err)
	}
	if s.reg, err = media.NewRegistry(cfg); err != nil {
		t.Fatal(err)
	}
	workers := pgtest.EmptySchema(t, ctx, s.pool)
	if err := workqueue.Migrate(ctx, s.pool, workers); err != nil {
		t.Fatal(err)
	}
	queue, err := workqueue.New(s.pool, s.reg, workers)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := media.NewPGJournal(s.pool, s.schema, queue)
	if err != nil {
		t.Fatal(err)
	}
	host := pgtest.EmptySchema(t, ctx, s.pool)
	if err := riverhelpers.ApplyMigrations(ctx, s.pool, host); err != nil {
		t.Fatal(err)
	}
	if s.jobs, err = media.NewJobs(media.JobsConfig{Store: env.Store, Registry: s.reg, Locker: media.PGLocker(s.pool), Journal: journal,
		Pool: s.pool, Processes: queue}); err != nil {
		t.Fatal(err)
	}
	s.ms = s.jobs.Manifests()
	if s.uploads, err = media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: s.ms, ProcessOnUpload: true}); err != nil {
		t.Fatal(err)
	}
	if s.reader, err = media.NewReader(media.ReaderOptions{Manifests: s.ms, Queue: queue,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signer}}); err != nil {
		t.Fatal(err)
	}
	if err := s.svc.Bind(versions.Media{Manifests: s.ms, Uploads: s.uploads, Jobs: s.jobs}); err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, s.pool, &river.Config{Schema: host, FetchPollInterval: 100 * time.Millisecond,
		FetchCooldown: 50 * time.Millisecond}, s.jobs.RiverJobs(), s.svc.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	w, err := worker.New(ctx, worker.Config{Pool: s.pool, Schema: workers, ContentSchema: s.schema, Store: env.Store, Kinds: s.reg,
		HostSchema: host, TempDir: t.TempDir(), Threads: 1, ImageWorkers: 1, VideoWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/media/upload/", http.StripPrefix("/media/upload", media.UploadHandler(s.uploads, media.UploadHandlerOptions{Auth: authn})))
	mux.Handle("/media/", http.StripPrefix("/media", s.reader.Handler(media.HandlerOptions{Auth: authn, Limit: media.RateLimit{Disabled: true}})))
	h, err := s.svc.Handler(authn, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/v1/contents/{id}/versions/", http.StripPrefix("/v1", h))
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) table(name string) string { return pgx.Identifier{s.schema, name}.Sanitize() }

func (s *site) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatalf("%s: %v", sql, err)
	}
}

// content creates a published content of kind under rule, created by
// s.creator.
func (s *site) content(kind, rule string) string {
	id := contentref.NewID()
	s.exec(`INSERT INTO `+s.table("content_items")+` (tenant_id, id, kind, state, published_at, access) VALUES ($1, $2, $3, 'published', now(), $4)`,
		s.tenant, id, kind, rule)
	s.exec(`INSERT INTO `+s.table("content_text")+` (tenant_id, content_id, language, title) VALUES ($1, $2, 'en', 'A Title')`, s.tenant, id)
	s.exec(`INSERT INTO `+s.table("content_creators")+` (tenant_id, content_id, user_id) VALUES ($1, $2, $3)`, s.tenant, id, s.creator.ID)
	return id
}

// version adds a published English version to content.
func (s *site) version(content string) versions.Target {
	id := contentref.NewID()
	s.exec(`INSERT INTO `+s.table("content_versions")+` (tenant_id, id, content_id, languages, state, published_at) VALUES ($1, $2, $3, '{en}', 'published', now())`,
		s.tenant, id, content)
	return versions.Target{Content: content, Version: id}
}

func (s *site) segment(v versions.Target, pos, start, end int, release *time.Time) {
	s.exec(`INSERT INTO `+s.table("content_segments")+` (tenant_id, version_id, position, start_index, end_index, title, release_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.tenant, v.Version, pos, start, end, fmt.Sprintf("Part %d", pos), release)
}

func (s *site) ref(kind string, t versions.Target) contentref.ContentRef {
	s.t.Helper()
	ref, err := s.reg.VersionRef(kind, t.Content, t.Version)
	if err != nil {
		s.t.Fatal(err)
	}
	return ref
}

// upload presigns and PUTs body like a browser, as actor, and commits it.
func (s *site) upload(actor access.Actor, ref contentref.ContentRef, path, typ string, body []byte) error {
	sum := sha256.Sum256(body)
	p, err := s.uploads.Presign(s.ctx, actor, media.PresignRequest{Ref: ref, Path: path, Type: typ, Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		return err
	}
	if p.Put != nil {
		req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		req.Header = p.Put.Header.Clone()
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("put: %d", resp.StatusCode)
		}
	}
	_, err = s.uploads.Commit(s.ctx, actor, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: p.Path, Blob: p.Blob}})
	return err
}

func (s *site) mustUpload(actor access.Actor, ref contentref.ContentRef, path, typ string, body []byte) {
	s.t.Helper()
	if err := s.upload(actor, ref, path, typ, body); err != nil {
		s.t.Fatalf("upload %s %s: %v", ref, path, err)
	}
}

func (s *site) commit(actor access.Actor, ref contentref.ContentRef, ops ...media.Op) {
	s.t.Helper()
	if _, err := s.uploads.Commit(s.ctx, actor, ref, uuid.NewString(), ops); err != nil {
		s.t.Fatal(err)
	}
}

func (s *site) manifest(ref contentref.ContentRef) *media.Manifest {
	s.t.Helper()
	m, _, err := s.ms.Get(s.ctx, ref)
	if err != nil {
		s.t.Fatal(err)
	}
	return m
}

// plan waits for v's reader plan for actor to satisfy ok.
func (s *site) plan(actor access.Actor, v versions.Target, ok func(*versions.ReaderPlan) bool) *versions.ReaderPlan {
	s.t.Helper()
	var p *versions.ReaderPlan
	eventually(s.t, "reader plan", 3*time.Minute, func() bool {
		var err error
		p, err = s.svc.Plan(s.ctx, actor, v)
		if errors.Is(err, versions.ErrNotReady) {
			return false
		} else if err != nil {
			s.t.Fatal(err)
		}
		return ok(p)
	})
	return p
}

// call sends an HTTP request as actor (zero: anonymous).
func (s *site) call(actor access.Actor, method, path string, body any) (*http.Response, []byte) {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, r)
	if actor.ID != "" {
		req.Header.Set("Authorization", "Bearer "+testauth.Token(actor))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func pngOf(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x) + seed, uint8(y) * seed, seed, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// wav is a short tone.
func wav(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "a.wav")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-ac", "1", "-ar", "8000", out).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg: %v %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func files(m *media.Manifest, prefix string) []string {
	var out []string
	for _, f := range m.Files {
		if f.IsUpload() && strings.HasPrefix(f.Path, prefix) {
			out = append(out, f.Path)
		}
	}
	return out
}
