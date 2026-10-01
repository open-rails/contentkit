// Command uploadtestserver serves the media upload and read APIs over an
// isolated MinIO/RGW namespace for the browser SDK's integration tests
// (sdk/upload/test) and e2e specs. It reads the CONTENTKIT_TEST_S3_*
// variables, presigns for -public (a proxy the test controls), prints
// "READY <url> <namespace>" and runs until stdin closes.
// CONTENTKIT_TEST_S3_BUCKET selects an existing bucket; cleanup removes only
// the test namespace.
//
//	/upload/...            the upload API; X-Test-Actor names the caller ("reader" may not upload)
//	/upload-on-upload/...  the same with ProcessOnUpload
//	/read/...              the read API; "reader" reads as a viewer, everyone else as an editor
//	GET /object?kind&id&path|public   {"size","sha256"} of a stored file: an
//	                       item's file by path (or stem), or a public name
//
// It has no libvips or ffmpeg: a stand-in worker (standIn) processes items.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
)

var images = []string{"image/png", "image/jpeg"}

// kinds exercise the SDK: ordered pages with a derived file and a cropped
// public cover, a capped kind with server-named inline images, and a video
// whose poster is a frame or an upload.
var kinds = []media.Kind{
	{Name: "gallery", KeepOriginals: true,
		Uploads: []media.Upload{
			{Path: "originals/{name}", Types: images, MaxBytes: 10 << 20},
			{Path: "cover", Types: images, MaxBytes: 10 << 20},
		},
		Private: []media.Private{{Name: "low", From: "originals/{name}", To: "low-res/{name}.webp", Image: &media.Image{Width: 1200, Height: 1200}}},
		Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{230, 460},
			Image: media.Image{Aspect: media.Ratio("3:1")}}}},
	{Name: "post",
		Uploads: []media.Upload{
			{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 2},
			{Path: "inline/{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 100},
		},
		Public: []media.Public{{Name: "inline", From: "inline/{name}", To: "{name}.webp", Image: media.Image{Width: 1600}}}},
	{Name: "video", KeepOriginals: true,
		Uploads: []media.Upload{
			{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 30},
			{Path: "poster", Types: images, MaxBytes: 10 << 20, Frames: "source"},
			{Path: "subs/{name}", Types: []string{"text/vtt", "application/x-subrip"}, MaxBytes: 1 << 20},
		},
		Public: []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{640}}}},
}

type allow struct{}

func (allow) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = access.Resolution{Visible: true, Accessible: true, Editor: !a.Anonymous && a.ID != "reader"}
	}
	return out, nil
}

func (allow) CanUpload(_ context.Context, a access.Actor, _ media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: a.ID != "reader", Owner: "owner"}, nil
}

type actorKey struct{}

// identity is the X-Test-Actor caller, for the read API.
type identity struct{}

func (identity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(access.Actor)
	return a, ok
}

func actor(r *http.Request) (access.Actor, bool) {
	id := r.Header.Get("X-Test-Actor")
	return access.Actor{ID: id, Kind: "user"}, id != ""
}

func withActor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := actor(r); ok {
			r = r.WithContext(context.WithValue(r.Context(), actorKey{}, a))
		}
		h.ServeHTTP(w, r)
	})
}

// uploadStore records this fixture's sessions; MinIO cannot list uploads by prefix.
type uploadStore struct {
	*mediaS3.Store
	mu      sync.Mutex
	uploads []multipart
}

type multipart struct{ key, id string }

func (s *uploadStore) CreateMultipart(ctx context.Context, key, contentType string) (string, error) {
	id, err := s.Store.CreateMultipart(ctx, key, contentType)
	if err == nil {
		s.mu.Lock()
		s.uploads = append(s.uploads, multipart{key, id})
		s.mu.Unlock()
	}
	return id, err
}

func main() {
	public := flag.String("public", "", "presign endpoint the client reaches (default: the S3 endpoint)")
	grace := flag.Duration("grace", 0, "sweep grace; short values let tests see uploads go stale")
	flag.Parse()
	ctx := context.Background()

	suffix := make([]byte, 6)
	_, err := rand.Read(suffix)
	must(err)
	namespace := "sdk-" + hex.EncodeToString(suffix)
	cfg := mediaS3.Config{
		Bucket:          os.Getenv("CONTENTKIT_TEST_S3_BUCKET"),
		Region:          os.Getenv("CONTENTKIT_TEST_S3_REGION"),
		Endpoint:        os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT"),
		PublicEndpoint:  *public,
		AccessKeyID:     os.Getenv("CONTENTKIT_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CONTENTKIT_TEST_S3_SECRET_KEY"),
		UsePathStyle:    true,
	}
	created := cfg.Bucket == ""
	if created {
		cfg.Bucket = "ck-" + namespace
	}
	s3store, err := mediaS3.New(cfg)
	must(err)
	store := &uploadStore{Store: s3store}
	if created {
		_, err = store.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &cfg.Bucket})
		must(err)
	}
	worker := &standIn{store: store}
	defer func() {
		worker.wg.Wait()
		if err := drop(store, namespace+"/", created); err != nil {
			log.Print(err)
			os.Exit(1)
		}
	}()
	must(store.Check(ctx, namespace+"/probe/"))
	log.Printf("upload fixture bucket=%s prefix=%s/", cfg.Bucket, namespace)

	reg, err := media.NewRegistry(media.Config{Namespace: namespace, BaseURL: "http://media.invalid", Kinds: kinds,
		Hooks: media.Hooks{Resolver: allow{}, CanUpload: allow{}}})
	must(err)
	manifests, err := media.NewManifests(store, reg, media.ManifestOptions{Locker: &procLocker{}})
	must(err)
	worker.reg, worker.manifests = reg, manifests
	key := token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}
	ring, err := token.NewRing(key, nil)
	must(err)
	reader, err := media.NewReader(media.ReaderOptions{Manifests: manifests, Queue: worker,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: key}})
	must(err)
	newUploads := func(onUpload bool) http.Handler {
		u, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: manifests, Tickets: &ring, Grace: *grace,
			Queue: worker, Frames: worker, ProcessOnUpload: onUpload,
			Commits: media.RateLimit{Disabled: true}}) // one server for the whole SDK suite
		must(err)
		return media.UploadHandler(u, media.UploadHandlerOptions{Actor: actor})
	}

	mux := http.NewServeMux()
	mux.Handle("/upload/", http.StripPrefix("/upload", newUploads(false)))
	mux.Handle("/upload-on-upload/", http.StripPrefix("/upload-on-upload", newUploads(true)))
	mux.Handle("/read/", http.StripPrefix("/read", withActor(reader.Handler(media.HandlerOptions{Identity: identity{}, Limit: media.RateLimit{Disabled: true}}))))
	mux.HandleFunc("GET /object", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ref, err := reg.Ref(q.Get("kind"), q.Get("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		item, _ := reg.Item(ref)
		key, err := item.Public(q.Get("public"))
		if !q.Has("public") {
			key, err = fileKey(r.Context(), manifests, item, q.Get("path"))
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		rc, _, err := store.Get(r.Context(), key, media.GetOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer rc.Close()
		h := sha256.New()
		n, err := io.Copy(h, rc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"size": n, "sha256": hex.EncodeToString(h.Sum(nil))})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	fmt.Printf("READY http://%s %s\n", ln.Addr(), namespace)

	// Exit when the test runner closes stdin (or dies), or after an hour.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Hour):
	}
	_ = srv.Close()
}

// fileKey is the key of the item's file at p, or of the upload whose stem p
// is: its blob, or its staged upload until placed.
func fileKey(ctx context.Context, manifests *media.Manifests, item media.Item, p string) (string, error) {
	m, _, err := manifests.Get(ctx, item.Ref())
	if err != nil {
		return "", err
	}
	for _, f := range m.Files {
		if f.Path == p || f.IsUpload() && trimExt(f.Path) == p {
			if f.Staged != "" {
				return item.Staged(f.Staged)
			}
			return item.Blob(f.Blob)
		}
	}
	return "", fmt.Errorf("no file %q", p)
}

func trimExt(p string) string { return p[:len(p)-len(path.Ext(p))] }

// drop removes only this fixture's uploads and object versions. Existing buckets stay intact.
func drop(store *uploadStore, prefix string, created bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, bucket := store.Client(), store.Bucket()
	var errs []error
	store.mu.Lock()
	uploads := append([]multipart(nil), store.uploads...)
	store.mu.Unlock()
	for _, u := range uploads {
		if err := store.AbortMultipart(ctx, u.key, u.id); err != nil && !errors.Is(err, media.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("list fixture versions: %w", err))...)
		}
		del := func(key, version *string) {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: key, VersionId: version}); err != nil {
				errs = append(errs, fmt.Errorf("delete fixture version %s: %w", *key, err))
			}
		}
		for _, v := range page.Versions {
			del(v.Key, v.VersionId)
		}
		for _, m := range page.DeleteMarkers {
			del(m.Key, m.VersionId)
		}
	}
	if created {
		if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); err != nil {
			errs = append(errs, fmt.Errorf("drop fixture bucket %s: %w", bucket, err))
		}
	}
	return errors.Join(errs...)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// procLocker serializes edits within this single process (no Postgres here).
type procLocker struct{ locks sync.Map }

func (l *procLocker) Lock(ctx context.Context, key string) (func(), error) {
	m, _ := l.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock, nil
}
