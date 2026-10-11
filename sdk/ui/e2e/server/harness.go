package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"

	contentkit "github.com/open-rails/contentkit"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
	"github.com/open-rails/contentkit/taxonomy"
)

// The deployment's names; compose.yaml gives the worker the same ones.
const (
	tenant       = "e2e" // the ContentKit tenant and media namespace
	schema       = "e2e" // ContentKit's tables
	authSchema   = "auth"
	riverSchema  = "e2e_river"        // the host's River: AuthKit's and media's jobs
	workerSchema = "e2e_media_worker" // the media worker's River
	apiPrefix    = "/api/contentkit"
	// onUploadPrefix serves a second upload API with ProcessOnUpload, as a
	// host splitting its mounts would.
	onUploadPrefix = "/api/contentkit-on-upload"
	// membersPrefix serves a second Runtime that takes nothing anonymous.
	membersPrefix = "/api/contentkit-members"
)

type harness struct {
	pool       *pgxpool.Pool
	auth       *authkit.Client
	outbox     *authtest.Outbox
	store      *mediaS3.Store
	reg        *media.Registry
	manifests  *media.Manifests
	ring       token.Ring
	urls       *contenturl.Store
	rt         *contentkit.Runtime
	members    *contentkit.Runtime // takes nothing anonymous
	onUpload   *media.Uploads
	river      *river.Client[pgx.Tx]
	items      *items
	faults     *faults
	avatars    *ckauthkit.Avatars
	router     *contenturl.Router
	s3Endpoint *url.URL
	bucket     string
	origin     string
	media      string
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
	h.avatars = &ckauthkit.Avatars{Directory: h.auth, Staff: permMedia}

	db := stdlib.OpenDBFromPool(h.pool)
	err = contentkit.Migrate(ctx, contentkit.MigrateConfig{DB: db, Schema: schema})
	_ = db.Close()
	if err != nil {
		return nil, fmt.Errorf("migrate contentkit: %w", err)
	}
	if err := workqueue.Migrate(ctx, h.pool, workerSchema); err != nil {
		return nil, fmt.Errorf("migrate media worker queue: %w", err)
	}
	if err := riverkit.ApplyMigrations(ctx, h.pool, riverSchema); err != nil {
		return nil, err
	}

	if h.store, err = openBucket(ctx, c); err != nil {
		return nil, err
	}
	kinds, err := os.ReadFile(c.kinds)
	if err != nil {
		return nil, err
	}
	mc, err := media.ParseConfig(kinds)
	if err != nil {
		return nil, err
	}
	mc.BaseURL, mc.Hooks = mediaOrigin, media.Hooks{Resolver: h, CanUpload: h}
	if h.reg, err = media.NewRegistry(mc); err != nil {
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
	jobs, err := media.NewJobs(media.JobsConfig{Store: h.store, Registry: h.reg, Locker: media.PGLocker(h.pool), Journal: journal, Processes: queue, Pool: h.pool})
	if err != nil {
		return nil, err
	}
	h.manifests = jobs.Manifests()
	signing, err := token.ParseKey(c.tokenKey)
	if err != nil {
		return nil, err
	}
	if h.ring, err = token.NewRing(signing, nil); err != nil {
		return nil, err
	}
	frames, err := video.NewFrames(h.store)
	if err != nil {
		return nil, err
	}
	newUploads := func(onUpload bool) (*media.Uploads, error) {
		return media.NewUploads(media.UploadOptions{Store: h.store, Manifests: h.manifests, Tickets: &h.ring, Frames: frames,
			ProcessOnUpload: onUpload, Commits: media.RateLimit{Disabled: true}})
	}
	uploads, err := newUploads(false)
	if err != nil {
		return nil, err
	}
	if h.onUpload, err = newUploads(true); err != nil {
		return nil, err
	}
	progress, err := workqueue.NewProgressSource(h.pool, workerSchema)
	if err != nil {
		return nil, err
	}
	// One-second token windows: a refreshed read signs a new token.
	reader, err := media.NewReader(media.ReaderOptions{Manifests: h.manifests, Queue: queue, Progress: progress,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signing, Window: time.Second},
		Issuance: media.Issuance{Disabled: true}})
	if err != nil {
		return nil, err
	}

	if h.urls, err = contenturl.New(contenturl.Options{Pool: h.pool, Schema: schema, Tenant: tenant}); err != nil {
		return nil, err
	}
	h.router, err = contenturl.NewRouter(h.urls, contenturl.RouterOptions{
		Routes:    contenturl.Routes{content.KindPost: "blog", "gallery": "g", "album": "post", "video": "watch", "tag": "tag"},
		Languages: []string{"en", "de", "ja"},
		Visibility: func(r *http.Request, l contenturl.Link) (contenturl.Visibility, error) {
			if l.ContentKind == content.KindPost {
				return h.rt.Content.PostVisibility(r.Context(), l.ContentID)
			}
			return contenturl.Visible, nil
		}})
	if err != nil {
		return nil, err
	}
	tax, err := taxonomy.New(taxonomy.Options{Pool: h.pool, Schema: schema, Tenant: tenant, Kinds: []string{"tag"}, Languages: []string{"en"}})
	if err != nil {
		return nil, err
	}
	newRuntime := func(anonymous content.Anonymous) (*contentkit.Runtime, error) {
		return contentkit.NewRuntime(ctx, contentkit.RuntimeConfig{
			EmbeddedConfig: contentkit.EmbeddedConfig{PG: h.pool, PGSchema: schema, Tenant: tenant},
			Content: content.Options{
				Identity: identity{}, Authz: h, Resolver: h,
				Users:     &ckauthkit.Authors{Directory: h.auth, Media: h.manifests},
				Moderator: moderator{}, Classifier: classifier{}, PostBodyProcessor: newPostHTML(),
				Media: &content.Media{Images: h.manifests, Folders: jobs, PostKind: postFolder, PollKind: pollFolder},
				// Commentable kinds; posts are content's own.
				ContentKinds: []string{"gallery", "album", "video", content.KindPost},
				Anonymous:    anonymous,
				// The suites test the bound; the default may change.
				CommentMaxLength: 400,
				Perms: content.Perms{PostWrite: "post", PollWrite: "poll", CommentModerate: "moderate", ModerationReview: "review",
					CommentBan: "ban", Taxonomy: "taxonomy"},
				// Generous for a shared suite; a test reaches the comment limit with its own account.
				Limits: content.Limits{Comment: []content.Rate{{Count: 20, Per: time.Minute}}},
			},
			Uploads: uploads, Reader: reader, ReadLimit: media.RateLimit{Disabled: true},
			Codes: h.router, Taxonomy: taxonomy.Handler(tax),
		})
	}
	// The main mount takes signed-out comments, reactions and votes; the
	// members mount, over the same schema, takes nothing anonymous.
	if h.rt, err = newRuntime(content.Anonymous{Comments: true, Reactions: true, Votes: true}); err != nil {
		return nil, err
	}
	if h.members, err = newRuntime(content.Anonymous{}); err != nil {
		return nil, err
	}

	if h.river, err = riverkit.New(ctx, h.pool, &river.Config{Schema: riverSchema, Queues: map[string]river.QueueConfig{},
		FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond},
		h.auth.RiverJobs(), jobs.RiverJobs()); err != nil {
		return nil, err
	}
	if err := h.river.Start(ctx); err != nil {
		return nil, err
	}
	if err := h.auth.Start(ctx, authkit.WithRiverClient(h.river)); err != nil {
		return nil, err
	}
	h.faults = newFaults(h)
	return h, nil
}

func (h *harness) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
func (h *harness) ref(kind, id string) (contentref.ContentRef, error) { return h.reg.Ref(kind, id) }

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
