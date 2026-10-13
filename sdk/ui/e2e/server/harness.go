package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/workqueue"
)

// The deployment's names; compose.yaml gives the worker the same ones.
const (
	tenant       = "e2e" // the ContentKit tenant and media namespace
	schema       = "e2e" // ContentKit's tables
	authSchema   = "auth"
	riverSchema  = "e2e_river"        // the host's River: AuthKit's and ContentKit's jobs
	workerSchema = "e2e_media_worker" // the media worker's River
	apiPrefix    = "/api/contentkit"
	// onUploadPrefix serves a second ContentKit whose uploads process as they
	// land, as a host splitting its mounts would.
	onUploadPrefix = "/api/contentkit-on-upload"
	// membersPrefix serves a ContentKit that takes nothing anonymous.
	membersPrefix = "/api/contentkit-members"
)

type harness struct {
	pool     *pgxpool.Pool
	auth     *authkit.Client
	outbox   *authtest.Outbox
	store    *mediaS3.Store
	ck       *contentkit.Engine
	members  *contentkit.Engine // takes nothing anonymous
	onUpload *contentkit.Engine // processes uploads as they land
	river    *river.Client[pgx.Tx]
	items    *items
	faults   *faults
	// Fixture views of ContentKit's storage, standing in for the media
	// worker: the registry, manifests and token ring.
	reg        *media.Registry
	manifests  *media.Manifests
	ring       token.Ring
	groups     *groups.Store // runs the recount job on demand
	s3Endpoint *url.URL
	bucket     string
	origin     string
	media      string
}

// site is the content model: the items tests create (/__test/items) and
// blogs, each a content kind whose media item tests use is its content-level
// one (kinds.json); comics and movies with traits, segments and every group
// kind; group kinds that between them declare roles, attributes, order,
// parents, scopes, windows and version-level memberships; a tier and a
// content filter.
var site = func() kinds.Config {
	one := kinds.VersionRules{Languages: "one"}
	grouped := []string{"tag", "artist", "series", "character", "voice_actor", "rail"}
	item := func(route, presentation string) kinds.Kind {
		return kinds.Kind{Route: route, Presentation: presentation, Versions: one, Groups: []string{"tag", "series", "character"}}
	}
	return kinds.Config{Languages: []string{"en", "de", "ja"}, Tiers: []string{"premium"}, TokenClaims: []string{"premium"},
		ContentFilters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}, Labels: map[string]string{"en": "Gore", "ja": "ゴア"}}},
		Kinds: map[string]kinds.Kind{
			"gallery": item("g", "reader"), "album": item("album", "reader"), "note": item("note", "reader"), "channel": item("channel", "reader"),
			"file": item("file", "reader"), "video": item("v", "player"), "watch": item("watch", "player"), "abr": item("abr", "player"),
			blogKind: {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}},
			"comic": {Route: "comics", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored", "uncensored"}},
				Segments: true, Groups: grouped, Sort: "newest",
				Attributes: map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}, Filterable: true}}},
			"movie": {Route: "movies", Presentation: "player", Versions: kinds.VersionRules{Languages: "many", Traits: []string{"uncensored"}},
				Groups: []string{"tag", "series", "rail"}},
		},
		GroupKinds: map[string]kinds.GroupKind{
			"tag": {Route: "tag", VersionLevel: true},
			"artist": {Route: "artists", Roles: []string{"publisher"}, Attributes: map[string]kinds.Attribute{
				"artist_type": {Type: "enum", Values: []string{"artist", "circle", "company"}},
				"active":      {Type: "bool"}, "works": {Type: "int"}, "debut": {Type: "time"}, "link": {Type: "string"}}},
			"series":      {Route: "series", Ordered: true, Parent: "series"},
			"character":   {Route: "characters", ScopedBy: "series"},
			"voice_actor": {Route: "voice-actors", VersionLevel: true},
			"rail":        {Ordered: true, Windows: true},
		}}
}

func newHarness(ctx context.Context, c config, mediaOrigin string) (_ *harness, err error) {
	h := &harness{items: newItems(), bucket: c.bucket, origin: c.origin, media: mediaOrigin}
	if h.s3Endpoint, err = url.Parse(c.s3Endpoint); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			h.close()
		}
	}()
	if h.pool, err = openPool(ctx, c.dsn); err != nil {
		return nil, err
	}
	if h.auth, h.outbox, err = newAuth(ctx, c.origin, h.pool); err != nil {
		return nil, fmt.Errorf("authkit: %w", err)
	}
	if err := riverkit.ApplyMigrations(ctx, h.pool, riverSchema); err != nil {
		return nil, err
	}
	if h.store, err = openBucket(ctx, c); err != nil {
		return nil, err
	}
	registry, err := os.ReadFile(c.kinds)
	if err != nil {
		return nil, err
	}
	mc, err := media.ParseConfig(registry)
	if err != nil {
		return nil, err
	}
	mc.BaseURL = mediaOrigin

	// One ContentKit per mount, over one schema, AuthKit passed straight in.
	cfg := contentkit.Config{Tenant: tenant, Database: contentkit.DatabaseConfig{Schema: schema, RiverSchema: riverSchema}, Content: site(),
		Interactions: contentkit.InteractionsConfig{
			Anonymous: content.Anonymous{Comments: true, Reactions: true, Votes: true},
			// The suites test the bound; the default may change.
			CommentMaxLength: 400,
			// Generous for a shared suite; a test reaches the comment limit with its own account.
			Limits: contentkit.LimitsConfig{Comment: []contentkit.Rate{{Count: 20, Per: time.Minute}}},
		},
		Media: &contentkit.MediaConfig{Config: mc, WorkerSchema: workerSchema,
			// One-second token windows: a refreshed read signs a new token.
			Delivery:    contentkit.DeliveryConfig{SigningKey: c.tokenKey, Window: time.Second},
			CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true},
			Issuance: contentkit.IssuanceConfig{Disabled: true}},
	}
	deps := contentkit.Deps{Postgres: h.pool, Auth: h.auth.Authenticator(), Users: h.auth.UserInfo(), Store: h.store,
		Moderator: moderator{}, Classifier: classifier{}, BodyProcessor: newPostHTML()}
	if h.ck, err = contentkit.New(ctx, cfg, deps); err != nil {
		return nil, err
	}
	members, onUpload := cfg, cfg
	members.Interactions.Anonymous = content.Anonymous{}
	uploading := *cfg.Media
	uploading.ProcessOnUpload = true
	onUpload.Media = &uploading
	if h.members, err = contentkit.New(ctx, members, deps); err != nil {
		return nil, err
	}
	if h.onUpload, err = contentkit.New(ctx, onUpload, deps); err != nil {
		return nil, err
	}

	fixture, err := media.ForContent(mc, site())
	if err != nil {
		return nil, err
	}
	fixture.Namespace = tenant
	if h.reg, err = media.NewRegistry(fixture); err != nil {
		return nil, err
	}
	queue, err := workqueue.New(h.pool, h.reg, workerSchema)
	if err != nil {
		return nil, err
	}
	journal, err := media.NewPGJournal(h.pool, schema, queue)
	if err != nil {
		return nil, err
	}
	if h.manifests, err = media.NewManifests(h.store, h.reg, media.ManifestOptions{Locker: media.PGLocker(h.pool), Journal: journal}); err != nil {
		return nil, err
	}
	if h.groups, err = groups.New(groups.Options{Pool: h.pool, Schema: schema, Tenant: tenant, Kinds: site()}); err != nil {
		return nil, err
	}
	signing, err := token.ParseKey(c.tokenKey)
	if err != nil {
		return nil, err
	}
	if h.ring, err = token.NewRing(signing, nil); err != nil {
		return nil, err
	}
	// The host's River runs AuthKit's jobs and the main mount's ContentKit
	// jobs; the other mounts run theirs on their own River in its schema,
	// as replicas would.
	if h.river, err = riverkit.New(ctx, h.pool, &river.Config{Schema: riverSchema, Queues: map[string]river.QueueConfig{},
		FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond},
		h.auth.RiverJobs(), h.ck.RiverJobs()); err != nil {
		return nil, err
	}
	if err := h.river.Start(ctx); err != nil {
		return nil, err
	}
	if err := h.auth.Start(ctx, authkit.WithRiverClient(h.river)); err != nil {
		return nil, err
	}
	if err := h.ck.Start(ctx, contentkit.WithRiverClient(h.river)); err != nil {
		return nil, err
	}
	for _, e := range []*contentkit.Engine{h.members, h.onUpload} {
		if err := e.Start(ctx); err != nil {
			return nil, err
		}
	}
	h.faults = newFaults(h)
	return h, nil
}

func (h *harness) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, e := range []*contentkit.Engine{h.ck, h.members, h.onUpload} {
		if err := e.Close(ctx); err != nil {
			log.Printf("close contentkit: %v", err)
		}
	}
	if h.river != nil {
		if err := h.river.Stop(ctx); err != nil {
			hard, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = h.river.StopAndCancel(hard)
			cancel()
		}
	}
	if h.auth != nil {
		if err := h.auth.Close(ctx); err != nil {
			log.Printf("close authkit: %v", err)
		}
	}
	if h.pool != nil {
		h.pool.Close()
	}
}

// ref is the item's reference in its kind's namespace.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 40
	deadline := time.Now().Add(2 * time.Minute)
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// openBucket opens the bucket, presigning for the app origin (/{bucket}/ is
// forwarded to MinIO there), and creates it once MinIO answers.
func openBucket(ctx context.Context, c config) (*mediaS3.Store, error) {
	store, err := mediaS3.New(mediaS3.Config{Bucket: c.bucket, Region: "us-east-1", Endpoint: c.s3Endpoint, PublicEndpoint: c.origin,
		AccessKeyID: c.s3Key, SecretAccessKey: c.s3Secret, UsePathStyle: true})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err = store.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &c.bucket})
		var owned *types.BucketAlreadyOwnedByYou
		if err == nil || errors.As(err, &owned) {
			return store, store.Check(ctx, "_probe/")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("minio: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
