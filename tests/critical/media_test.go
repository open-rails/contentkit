package critical_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
	_ "golang.org/x/image/webp"
)

type mediaWorld struct {
	store         *medias3.Store
	bucket        string
	gateway       *httptest.Server
	key           token.Key
	signing       string
	worker        *worker.Worker
	cancel        context.CancelFunc
	workerDone    chan error
	management    *awss3.Client
	gatewayConfig gateway.Config
}

func newMediaStorage(t *testing.T, scope ck.Scope) *mediaWorld {
	t.Helper()
	endpoint := os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("CONTENTKIT_CRITICAL_S3_ENDPOINT is required for selected real-media scenarios")
	}
	user, password := os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD")
	if user == "" || password == "" {
		t.Fatal("owned object-store credentials are required")
	}
	m := &mediaWorld{bucket: "ck-critical-" + uuid.NewString()}
	management := awss3.New(awss3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(user, password, ""), RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired})
	m.management = management
	_, err := management.CreateBucket(t.Context(), &awss3.CreateBucketInput{Bucket: &m.bucket})
	must(t, err)
	m.store, err = medias3.New(medias3.Config{Bucket: m.bucket, Endpoint: endpoint, AccessKeyID: user, SecretAccessKey: password, UsePathStyle: true})
	must(t, err)
	secret := make([]byte, 32)
	_, err = rand.Read(secret)
	must(t, err)
	signing := "critical:" + base64.StdEncoding.EncodeToString(secret)
	m.signing = signing
	m.key, err = token.ParseKey(signing)
	must(t, err)
	ring, err := token.NewRing(m.key, nil)
	must(t, err)
	m.gatewayConfig = gateway.Config{Endpoint: endpoint, AccessKeyID: user, SecretAccessKey: password, Ring: ring,
		Routes: []gateway.Route{{Host: "127.0.0.1", Scope: layout.Scope{TenantID: scope.TenantID.String(), AppID: scope.AppID.String()}, Bucket: m.bucket, Origins: []string{"https://critical.invalid"}}}}
	handler, err := gateway.New(m.gatewayConfig)
	must(t, err)
	m.gateway = httptest.NewServer(handler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m.gateway.Close()
		for {
			objects, err := management.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: &m.bucket})
			if err != nil {
				t.Errorf("list owned media cleanup: %v", err)
				return
			}
			if len(objects.Contents) == 0 {
				break
			}
			keys := []types.ObjectIdentifier{}
			for _, object := range objects.Contents {
				keys = append(keys, types.ObjectIdentifier{Key: object.Key})
			}
			if _, err := management.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Bucket: &m.bucket, Delete: &types.Delete{Objects: keys}}); err != nil {
				t.Errorf("delete owned media: %v", err)
				return
			}
		}
		if _, err := management.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: &m.bucket}); err != nil {
			t.Errorf("delete owned bucket: %v", err)
		}
	})
	return m
}

func attachMedia(t *testing.T, w *world, a *application, configure ...func(*ck.Config)) *mediaWorld {
	t.Helper()
	m := newMediaStorage(t, *a.cfg.Scope)
	var err error
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	// Poll promptly without changing commit or capability-issuance limits.
	a.cfg.Media = &ck.MediaConfig{Config: media.Config{BaseURL: m.gateway.URL, Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 4}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "attachments/{name}", Types: []string{"text/plain"}, MaxBytes: 80 << 20, Max: 2}},
		Private: []media.Private{{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{16, 32}, Image: media.Image{Aspect: media.Square}}},
	}}}, ReadLimit: ck.RateLimitConfig{Disabled: true}, WorkerSchema: w.schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: m.signing, TTL: time.Minute, Window: time.Second}}
	for _, apply := range configure {
		apply(&a.cfg)
	}
	accountStorage, accountPrimary, err := a.cfg.AccountMedia()
	must(t, err)
	if accountPrimary {
		m.gatewayConfig.Routes = append(m.gatewayConfig.Routes, gateway.Route{Host: "127.0.0.1", Scope: layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}, Bucket: m.bucket})
		handler, err := gateway.New(m.gatewayConfig)
		must(t, err)
		m.gateway.Close()
		m.gateway = httptest.NewServer(handler)
		a.cfg.Media.BaseURL = m.gateway.URL
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{accountStorage.WorkerSchema}.Sanitize()+" CASCADE")
			must(t, err)
		})
	}
	a.deps.Store = m.store
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	routes, err := a.engine.Routes(ck.Routes{Prefix: "/content", Groups: a.engine.RouteGroups()})
	must(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
	}
	a.server = httptest.NewServer(mux)
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	workerConfig := worker.Config{Pool: w.pool, Schema: a.cfg.Media.WorkerSchema, ContentSchema: w.schema, HostSchema: w.schema + "_river", Store: m.store, Kinds: registry, Threads: 1, IngestWorkers: 1, ImageSources: 1, DerivativeWorkers: 1, ShutdownGrace: time.Second}
	if accountPrimary {
		workerConfig.AccountSchema, workerConfig.AccountWorkerSchema = accountStorage.Schema, accountStorage.WorkerSchema
	}
	m.worker, err = worker.New(t.Context(), workerConfig)
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.workerDone = make(chan error, 1)
	go func() { m.workerDone <- m.worker.Run(run) }()
	must(t, a.engine.StartJobs(run))
	t.Cleanup(func() {
		a.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.engine.Close(ctx); err != nil {
			t.Errorf("drain media engine: %v", err)
		}
		m.cancel()
		select {
		case err := <-m.workerDone:
			if err != nil {
				t.Errorf("stop owned media worker: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("owned media worker failed to stop")
		}
	})
	return m
}

func imageBytes(t testing.TB) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 2), G: uint8(y * 3), B: 40, A: 255})
		}
	}
	var encoded bytes.Buffer
	must(t, png.Encode(&encoded, img))
	return encoded.Bytes()
}
