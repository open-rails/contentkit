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
	"maps"
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
	"github.com/open-rails/contentkit/internal/resolve"
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
// previews, audio, archives and downloads, each version with its cover;
// movies with an HLS ladder, subtitles and audio tracks per version; and
// people's avatars.
func mediaKinds(ns string, hooks media.Hooks) media.Config {
	return media.Config{Namespace: ns, BaseURL: "https://media.example", Hooks: hooks,
		Avatars: media.Avatars{Kind: "user", Preset: "avatar"},
		Kinds: []media.Kind{
			{Name: "gallery", Versions: true, KeepOriginals: true,
				Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"low", "thumb"}, Previews: "preview",
					Playback: "audio/{name}", Import: "import/{name}", Download: "zip"},
				Uploads: []media.Upload{
					{Path: "cover", Types: pngs, MaxBytes: 10 << 20},
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
				},
				Public: []media.Public{
					{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{64}},
					{Name: "preview", From: "previews/{name}", To: "preview-{n}.webp", First: 3},
				}},
			{Name: "movie", Versions: true, KeepOriginals: true,
				Uploads: []media.Upload{
					{Path: "cover", Types: pngs, MaxBytes: 10 << 20},
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
				Public:  []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{32}, Stable: true}}},
		}}
}

// siteKinds is the site's one kinds list: galleries and movies, tags and
// user-owned channels.
var siteKinds = kinds.Config{Languages: []string{"en"}, Tiers: []string{"premium"}, TokenClaims: []string{"premium"},
	Kinds: map[string]kinds.Kind{
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Segments: true, Entities: []string{"tag", "channel"}},
		"movie":   {Route: "m", Presentation: "player", Versions: kinds.VersionRules{Languages: "many"}},
	},
	EntityKinds: map[string]kinds.EntityKind{"tag": {Route: "tags"}, "channel": {Owner: "user", AuthScope: true}}}

// grants are the test auth's permissions: staff holds root:entities:write;
// in a channel's AuthKit scope, its editor holds channel:media:write.
type grants struct {
	staff  string
	scoped map[string]string // scope id -> editor id
}

func (g *grants) Can(context.Context, access.Actor, string) (bool, error) { return false, nil }

func (g *grants) CanIn(_ context.Context, a access.Actor, scope auth.Scope, perm string) (bool, error) {
	if scope == testauth.Scope {
		return a.ID == g.staff && perm == string(access.EntitiesWrite), nil
	}
	return g.scoped[scope.ID] == a.ID && perm == "channel:media:write", nil
}

// catalog stands in for the catalog's resolver over the gate (lanes
// 120-catalog and 121-access implement it) on the contract tables: a version
// is live when it and its content are published, due and not deleted;
// staff and creators edit; its effective rule opens when free or when the
// viewer holds one of its tiers.
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

func (c *catalog) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	out := map[contentref.ContentKey]resolve.Resolution{}
	for _, ref := range refs {
		var live, creator bool
		var content access.Rule
		var override *access.Rule
		err := c.pool.QueryRow(ctx, `SELECT c.state = 'published' AND c.deleted_at IS NULL AND
	($3 = '' OR v.state = 'published' AND (v.publish_at IS NULL OR v.publish_at <= now()) AND v.deleted_at IS NULL),
	EXISTS (SELECT 1 FROM `+c.t("content_creators")+` cr WHERE cr.tenant_id = c.tenant_id AND cr.content_id = c.id AND cr.user_id = $4),
	c.access, v.access
FROM `+c.t("content_items")+` c LEFT JOIN `+c.t("content_versions")+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id AND v.id = $3
WHERE c.tenant_id = $1 AND c.id = $2 AND c.kind = $5 AND ($3 = '' OR v.id IS NOT NULL)`,
			c.tenant, ref.ContentID, ref.Version(), actor.ID, ref.ContentKind).Scan(&live, &creator, &content, &override)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		rule := access.Effective(content, override)
		held := c.tiers[actor.ID]
		opens := rule.Free(time.Now()) || slices.ContainsFunc(rule.Tiers, func(t string) bool { return !actor.Anonymous && slices.Contains(held, t) })
		editor := actor.Person() && (creator || actor.ID == c.staff)
		out[ref.Key()] = resolve.Resolution{Visible: live || editor, Accessible: opens, Editor: editor}
	}
	return out, nil
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

// marker is a host download policy: it marks page 2 (and 9, past the end)
// of premium versions by appending the viewer's id.
type marker struct{}

func (marker) Decide(_ context.Context, r versions.DownloadRequest) (versions.DownloadDecision, error) {
	if r.Rule.Level != access.TierLevel {
		return versions.DownloadDecision{Allowed: true}, nil
	}
	return versions.DownloadDecision{Allowed: true, Mark: []int{2, 9}}, nil
}

func (marker) Mark(_ context.Context, a access.Actor, _ versions.Target, _ int, page []byte) ([]byte, string, error) {
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
	workers string // the worker's River schema
	host    string // the host's River schema

	staff, creator, fan, stranger access.Actor
}

type siteOptions struct {
	policy    versions.DownloadPolicy
	downloads versions.Downloads
	// gallery edits the site's gallery kind.
	gallery func(*kinds.Kind)
	// mediaGallery adjusts a gallery's media rules before the registry is built.
	mediaGallery func(*media.Kind)
	// reader adjusts delivery and URL-window limits before workers start.
	reader func(*media.ReaderOptions)
	// workerStore intercepts the standalone worker storage in protocol tests.
	workerStore func(*site) media.Store
}

func newSite(t *testing.T, o siteOptions) *site {
	t.Helper()
	env := s3test.Open(t)
	if !env.Caps().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	s := &site{Env: env, t: t, ctx: ctx, tenant: env.Tenant, pool: env.Pool(), schema: env.ContentSchema()}
	person := func() access.Actor { return access.Actor{ID: contentref.NewID(), Kind: "user"} }
	s.staff, s.creator, s.fan, s.stranger = person(), person(), person(), person()
	s.cat = &catalog{pool: s.pool, schema: s.schema, tenant: s.tenant, staff: s.staff.ID,
		tiers: map[string][]string{s.fan.ID: {"premium"}}}
	s.grants = &grants{staff: s.staff.ID, scoped: map[string]string{}}
	authn := testauth.Auth{Authz: s.grants, Claims: map[string][]string{s.fan.ID: {"premium"}}}
	siteKinds := siteKinds
	if o.gallery != nil {
		siteKinds.Kinds = maps.Clone(siteKinds.Kinds)
		g := siteKinds.Kinds["gallery"]
		o.gallery(&g)
		siteKinds.Kinds["gallery"] = g
	}
	gate, err := access.NewGate(access.GateConfig{Tenant: s.tenant, Kinds: siteKinds})
	if err != nil {
		t.Fatal(err)
	}
	s.svc, err = versions.New(versions.Options{Tenant: s.tenant, Schema: s.schema, Pool: s.pool, Kinds: siteKinds, Auth: authn,
		Resolver: s.cat, Gate: gate, Facets: s.cat, Policy: o.policy, Downloads: o.downloads})
	if err != nil {
		t.Fatal(err)
	}
	mediaConfig := mediaKinds(s.tenant, s.svc.Hooks())
	if o.mediaGallery != nil {
		for i := range mediaConfig.Kinds {
			if mediaConfig.Kinds[i].Name == "gallery" {
				o.mediaGallery(&mediaConfig.Kinds[i])
			}
		}
	}
	cfg, err := media.ForContent(mediaConfig, siteKinds)
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
	s.workers, s.host = workers, host
	if s.jobs, err = media.NewJobs(media.JobsConfig{Store: env.Store, Registry: s.reg, Locker: media.PGLocker(s.pool), Journal: journal,
		Pool: s.pool, Processes: queue}); err != nil {
		t.Fatal(err)
	}
	s.ms = s.jobs.Manifests()
	if s.uploads, err = media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: s.ms, ProcessOnUpload: true}); err != nil {
		t.Fatal(err)
	}
	readerOptions := media.ReaderOptions{Manifests: s.ms, Queue: queue,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signer}}
	if o.reader != nil {
		o.reader(&readerOptions)
	}
	if s.reader, err = media.NewReader(readerOptions); err != nil {
		t.Fatal(err)
	}
	if err := s.svc.Bind(versions.Media{Manifests: s.ms, Uploads: s.uploads, Jobs: s.jobs, Reader: s.reader}); err != nil {
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
	// The stock worker receives plain registry data, not in-process service
	// hooks. Its publication retirement barrier must be installed by New.
	workerConfig := s.reg.Config()
	workerConfig.Hooks = media.Hooks{}
	workerRegistry, err := media.NewRegistry(workerConfig)
	if err != nil {
		t.Fatal(err)
	}
	var workerStore media.Store = env.Store
	if o.workerStore != nil {
		workerStore = o.workerStore(s)
	}
	w, err := worker.New(ctx, worker.Config{Pool: s.pool, Schema: workers, ContentSchema: s.schema, Store: workerStore, Kinds: workerRegistry,
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
	t.Cleanup(func() {
		if t.Failed() {
			s.dumpJobs()
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

// placements counts the worker's place jobs by item kind.
func (s *site) placements() map[string]int {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `SELECT args->'ref'->>'content_kind', count(*) FROM `+pgx.Identifier{s.workers, "river_job"}.Sanitize()+`
WHERE kind = 'contentkit_media_ingest' GROUP BY 1`)
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			s.t.Fatal(err)
		}
		out[kind] = n
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// placing counts ref's applied commits that asked the worker to place.
func (s *site) placing(ref contentref.ContentRef) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM `+s.table("content_media_commits")+`
WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3 AND content_version_id IS NOT DISTINCT FROM $4
AND state = 'applied' AND (effects->'process'->>'place')::boolean`, ref.TenantID, ref.ContentKind, ref.ContentID, ref.ContentVersionID).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// settles waits for both publishers and media workers to drain. A publisher
// can legitimately enqueue media after the worker first goes idle. Once both
// queues settle, no new worker job should arrive during a publisher poll.
func (s *site) settles() {
	s.t.Helper()
	sql := `SELECT w.waiting, h.waiting, w.last FROM
 (SELECT count(*) FILTER (WHERE state IN ('available', 'pending', 'running', 'retryable', 'scheduled')) AS waiting,
 coalesce(max(id), 0) AS last FROM ` + pgx.Identifier{s.workers, "river_job"}.Sanitize() + `) w
 CROSS JOIN (SELECT count(*) FILTER (WHERE state IN ('available', 'pending', 'running', 'retryable', 'scheduled')) AS waiting
 FROM ` + pgx.Identifier{s.host, "river_job"}.Sanitize() + `) h`
	var workerWaiting, hostWaiting, last int64
	eventually(s.t, "the publisher and worker queues to drain", time.Minute, func() bool {
		if err := s.pool.QueryRow(s.ctx, sql).Scan(&workerWaiting, &hostWaiting, &last); err != nil {
			s.t.Fatal(err)
		}
		return workerWaiting == 0 && hostWaiting == 0
	})
	time.Sleep(8 * time.Second)
	var now int64
	if err := s.pool.QueryRow(s.ctx, sql).Scan(&workerWaiting, &hostWaiting, &now); err != nil {
		s.t.Fatal(err)
	}
	if workerWaiting != 0 || hostWaiting != 0 || now != last {
		s.t.Fatalf("the settled version has %d worker and %d host jobs pending, and got %d more worker jobs", workerWaiting, hostWaiting, now-last)
	}
}

// lockVersion holds v's content and version rows, in the order catalog
// writes take them, until the returned transaction ends; pid is its backend.
func (s *site) lockVersion(v versions.Target, version bool) (tx pgx.Tx, pid uint32) {
	s.t.Helper()
	tx, err := s.pool.Begin(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tx.QueryRow(s.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		s.t.Fatal(err)
	}
	if _, err := tx.Exec(s.ctx, `SELECT FROM `+s.table("content_items")+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.tenant, v.Content); err != nil {
		s.t.Fatal(err)
	}
	if version {
		if _, err := tx.Exec(s.ctx, `SELECT FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.tenant, v.Version); err != nil {
			s.t.Fatal(err)
		}
	}
	return tx, pid
}

// waitBlocked waits until some backend waits on pid's locks and reports
// true, or reports false once done receives first.
func (s *site) waitBlocked(pid uint32, done <-chan error) bool {
	s.t.Helper()
	for {
		select {
		case err := <-done:
			s.t.Logf("returned without waiting: %v", err)
			return false
		default:
		}
		var blocked bool
		if err := s.pool.QueryRow(s.ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			s.t.Fatal(err)
		}
		if blocked {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dumpJobs logs the worker's and the host's River jobs: what a timed-out
// wait was waiting on.
func (s *site) dumpJobs() {
	at := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format("15:04:05.000")
	}
	for name, schema := range map[string]string{"worker": s.workers, "host": s.host} {
		rows, err := s.pool.Query(context.Background(), `SELECT id, kind, state, attempt, args::text, coalesce(errors[array_length(errors, 1)]::text, ''),
	created_at, attempted_at, finalized_at FROM `+pgx.Identifier{schema, "river_job"}.Sanitize()+` ORDER BY id`)
		if err != nil {
			s.t.Logf("%s jobs: %v", name, err)
			continue
		}
		for rows.Next() {
			var (
				id, attempt                int64
				kind, state, args, lastErr string
				created                    time.Time
				attempted, finalized       *time.Time
			)
			if err := rows.Scan(&id, &kind, &state, &attempt, &args, &lastErr, &created, &attempted, &finalized); err != nil {
				s.t.Logf("%s jobs: %v", name, err)
				break
			}
			s.t.Logf("%s job %d %s %s attempt=%d created=%s attempted=%s finalized=%s %s %.200s", name, id, kind, state, attempt,
				at(&created), at(attempted), at(finalized), args, lastErr)
		}
		rows.Close()
	}
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

// version adds a published English version to content, last, with the
// listing projection the catalog would write.
func (s *site) version(content string) versions.Target {
	id := contentref.NewID()
	s.exec(`INSERT INTO `+s.table("content_versions")+` (tenant_id, id, content_id, position, languages, state, published_at)
SELECT $1, $2, $3, coalesce((SELECT max(position) + 1 FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND content_id = $3), 0), '{en}', 'published', now()`,
		s.tenant, id, content)
	s.exec(`INSERT INTO `+s.table("content_facets")+` (tenant_id, content_id, version_id, kind, live_from, languages, access_level, position)
SELECT v.tenant_id, v.content_id, v.id, c.kind, now(), v.languages, 'public', v.position FROM `+s.table("content_versions")+` v
JOIN `+s.table("content_items")+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id WHERE v.tenant_id = $1 AND v.id = $2`, s.tenant, id)
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
