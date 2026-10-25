package critical_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/worker"
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

func attachMedia(t *testing.T, w *world, a *application) *mediaWorld {
	t.Helper()
	m := newMediaStorage(t, *a.cfg.Scope)
	var err error
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Media = &ck.MediaConfig{Config: media.Config{BaseURL: m.gateway.URL, Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
		Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 4}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "attachments/{name}", Types: []string{"text/plain"}, MaxBytes: 80 << 20, Max: 2}},
		Private: []media.Private{{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{16, 32}, Image: media.Image{Aspect: media.Square}}},
	}}}, WorkerSchema: w.schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: m.signing, TTL: time.Minute, Window: time.Second}}
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
	registry, err := media.NewRegistry(config)
	must(t, err)
	m.worker, err = worker.New(t.Context(), worker.Config{Pool: w.pool, Schema: a.cfg.Media.WorkerSchema, ContentSchema: w.schema, HostSchema: w.schema + "_river", Store: m.store, Kinds: registry, Threads: 1, ImageWorkers: 1, ImageSources: 1, VideoWorkers: 1, ShutdownGrace: time.Second})
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.workerDone = make(chan error, 1)
	go func() { m.workerDone <- m.worker.Run(run) }()
	must(t, a.engine.Start(run))
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

func TestRealMediaIngest(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	admin := a.engine.Client()
	t.Run("CK-J09-S01_real_image_upload_produces_a_readable_rendition", func(t *testing.T) {
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Image processing"}}})
		must(t, err)
		if len(item.Versions) != 1 {
			t.Fatal("gallery fixture lacks its edition")
		}
		ref := media.RefBody{Kind: "gallery", ID: item.ID, Version: item.Versions[0].ID}
		payload := imageBytes(t)
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}, {Path: "cover.png", Type: "image/png", Body: bytes.NewReader(payload)}})
		must(t, err)
		var ready media.ReadResult
		waitFor(t, func() bool {
			ready, err = admin.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
			must(t, err)
			for _, file := range ready.Files {
				if file.Path == "reader/one.webp" && file.URL != "" {
					return true
				}
			}
			return false
		})
		_, err = a.client("").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		waitFor(t, func() bool {
			ready, err = admin.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
			must(t, err)
			return len(ready.Public) == 1 && len(ready.Public[0].Renditions) == 2 && ready.State == media.StateReady
		})
		if len(ready.Public) != 1 || len(ready.Public[0].Renditions) != 2 {
			t.Fatalf("cover renditions incomplete: %+v", ready.Public)
		}
		seen := map[int]bool{}
		for _, rendition := range ready.Public[0].Renditions {
			assertImage(t, rendition.URL, rendition.W, rendition.H)
			if rendition.W != rendition.H || (rendition.W != 16 && rendition.W != 32) {
				t.Fatalf("unexpected cover dimensions: %+v", rendition)
			}
			seen[rendition.W] = true
		}
		if !seen[16] || !seen[32] {
			t.Fatal("cover widths missing")
		}
		for _, file := range ready.Files {
			if file.Path != "reader/one.webp" {
				continue
			}
			res, err := http.Get(file.URL)
			must(t, err)
			data, err := io.ReadAll(res.Body)
			res.Body.Close()
			must(t, err)
			if res.StatusCode != 200 {
				t.Fatalf("actual image rendition returned %d", res.StatusCode)
			}
			decoded, format, err := image.DecodeConfig(bytes.NewReader(data))
			must(t, err)
			if format != "webp" || decoded.Width != 48 || decoded.Height != 32 {
				t.Fatalf("actual rendition = %s %dx%d", format, decoded.Width, decoded.Height)
			}
		}
	})
}
