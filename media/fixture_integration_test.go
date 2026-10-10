package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/workqueue"
)

const mediaHost = "media.doujins.test"

var signKey = token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

// cid is the n-th test item id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func blobOf(b []byte) string {
	sum := sha256.Sum256(b)
	return layout.BlobName(sum[:], cid(1)) // synthetic names only; real writes use NewBlob
}

func matchesBlob(name string, body []byte) bool {
	sum, ok := layout.BlobDigest(name)
	want := sha256.Sum256(body)
	return ok && bytes.Equal(sum, want[:])
}

// blob models a producer: record a fresh allocation before sending bytes.
func (f *fixture) blob(ref contentref.ContentRef, body []byte, typ string) string {
	f.t.Helper()
	sum := sha256.Sum256(body)
	name, err := f.ms.NewBlob(f.t.Context(), ref, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	key, _ := item.Blob(name)
	if _, err := f.env.Store.Put(f.t.Context(), key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: typ}); err != nil {
		f.t.Fatal(err)
	}
	return name
}

func (f *fixture) fileBlob(ref contentref.ContentRef, path string) string {
	f.t.Helper()
	m, _, err := f.ms.Get(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	file, ok := m.Get(path)
	if !ok {
		f.t.Fatalf("no file %s", path)
	}
	return file.Blob
}

func (f *fixture) editorView(ref contentref.ContentRef, path string) string {
	f.t.Helper()
	name := f.blob(ref, []byte("editor view"), "image/webp")
	if _, err := f.ms.EditExisting(f.t.Context(), ref, func(m *media.Manifest) error {
		i := m.Find(path)
		m.Files[i].Editor = &media.EditorImage{Blob: name, FP: f.reg.EditorFingerprint(m.Files[i])}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return name
}

var images = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// testConfig is an app's registry: galleries of pages with a cover, posts
// with inline images, videos, and a shared account kind.
func testConfig(ns, shared string) media.Config {
	return media.Config{Namespace: ns, BaseURL: "https://" + mediaHost, Kinds: []media.Kind{
		{Name: "gallery", KeepOriginals: true,
			Uploads: []media.Upload{
				{Path: "originals/{name}", Types: images, MaxBytes: 10 << 20},
				{Path: "cover", Types: images, MaxBytes: 10 << 20},
				{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 256 << 20, Max: 2},
			},
			Private: []media.Private{
				{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 460, Height: 650, Fit: media.FitCover}},
				{Name: "high", From: "originals/{name}", To: "high/{name}.webp", Image: &media.Image{Quality: 90}},
				{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "high/"},
			},
			Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{230, 460},
				Image: media.Image{Aspect: media.Ratio("46:65"), MinWidth: 100}, Default: "cover.png"}},
		},
		{Name: "post", ServeOriginals: true,
			Uploads: []media.Upload{{Path: "inline/{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 100}},
			Public:  []media.Public{{Name: "inline", From: "inline/{name}", To: "{name}.webp", Image: media.Image{Width: 1600, Height: 1600}}},
		},
		{Name: "video", KeepOriginals: true,
			Uploads: []media.Upload{
				{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 30},
				{Path: "subs/{name}", Types: media.SubtitleTypes, MaxBytes: 1 << 20},
				{Path: "poster", Types: images, MaxBytes: 10 << 20, Frames: "source"},
			},
			Private: []media.Private{
				{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{Ladder: []int{1080, 480}}},
				{Name: "mp4-1080", From: "source", To: "video/source-1080p.mp4", Download: "{title} (1080p).mp4", MP4: media.Rung(1080)},
				{Name: "vtt", From: "subs/{name}", To: "vtt/{name}.vtt", Subtitles: &media.Subtitles{}},
			},
			Public: []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{640}}},
		},
		{Name: "user", Namespace: shared,
			Uploads: []media.Upload{{Path: "avatar", Types: images, MaxBytes: 10 << 20}},
			Public: []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{64, 128},
				Image: media.Image{Aspect: media.Square, Animation: media.AnimationReject}, Default: "avatar.png"}}},
	}}
}

// miniRegistry is one plain kind, "post", in namespace ns.
func miniRegistry(t testing.TB, ns string) *media.Registry {
	t.Helper()
	r, err := media.NewRegistry(media.Config{Namespace: ns, Kinds: []media.Kind{
		{Name: "post", Uploads: []media.Upload{{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// resolver is the app's ContentResolver: verdicts per item id; anonymous
// viewers get anon.
type resolver struct {
	mu       sync.Mutex
	calls    atomic.Int32
	verdicts map[string]access.Resolution
	anon     map[string]access.Resolution
	err      error
}

func (r *resolver) set(id string, res access.Resolution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verdicts[id] = res
	r.anon[id] = access.Resolution{Visible: res.Visible, Accessible: res.Accessible}
}

func (r *resolver) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		if a.Anonymous {
			out[ref.Key()] = r.anon[ref.ContentID]
		} else {
			out[ref.Key()] = r.verdicts[ref.ContentID]
		}
	}
	return out, nil
}

// authorizer lets every signed-in actor but "reader" upload; owner "owner";
// "staff" is exempt.
type authorizer struct {
	mu      sync.Mutex
	targets []media.UploadTarget
}

func (a *authorizer) CanUpload(_ context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	a.mu.Lock()
	a.targets = append(a.targets, t)
	a.mu.Unlock()
	return media.UploadGrant{Allowed: actor.ID != "reader" && !actor.Anonymous, Owner: "owner", Exempt: actor.ID == "staff"}, nil
}

// queue reads the real jobs committed into the fixture's worker schema.
type queue struct {
	*workqueue.Queue
	t    *testing.T
	pool *pgxpool.Pool
}

func (q *queue) take() []media.ProcessJob {
	q.t.Helper()
	rows, err := q.pool.Query(q.t.Context(), `WITH taken AS (DELETE FROM `+pgx.Identifier{q.Schema(), "river_job"}.Sanitize()+`
WHERE state IN ('available', 'scheduled', 'retryable') RETURNING id, kind, args)
SELECT kind, args FROM taken ORDER BY id`)
	if err != nil {
		q.t.Fatal(err)
	}
	defer rows.Close()
	var out []media.ProcessJob
	for rows.Next() {
		var kind string
		var args []byte
		if err := rows.Scan(&kind, &args); err != nil {
			q.t.Fatal(err)
		}
		var job media.ProcessJob
		if err := json.Unmarshal(args, &job); err != nil {
			q.t.Fatal(err)
		}
		job.Place = kind == (workqueue.PlaceArgs{}).Kind()
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		q.t.Fatal(err)
	}
	return out
}

// fixture is one test's media stack on real MinIO (and PostgreSQL for the
// manifest lock): the app's registry, uploads, reads and jobs, and the
// media gateway serving its URLs.
type fixture struct {
	t       *testing.T
	env     *s3test.Env
	ns      string
	shared  string
	reg     *media.Registry
	ms      *media.Manifests
	up      *media.Uploads
	rd      *media.Reader
	jobs    *media.Jobs
	res     *resolver
	auth    *authorizer
	q       *queue
	journal *media.PGJournal
	purged  chan []string
	gateway *httptest.Server
	editor  access.Actor
}

func newFixture(t *testing.T) *fixture {
	return newFixtureOn(t, s3test.Open(t), nil)
}

func newFixtureOn(t *testing.T, env *s3test.Env, mutate func(*media.Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, env: env, ns: env.Tenant, shared: "acct" + strings.ReplaceAll(env.Tenant, "-", ""),
		res:  &resolver{verdicts: map[string]access.Resolution{}, anon: map[string]access.Resolution{}},
		auth: &authorizer{}, purged: make(chan []string, 64), editor: access.Actor{ID: "editor", Kind: "user"}}
	t.Cleanup(func() { f.drop(f.shared + "/") })
	cfg := testConfig(f.ns, f.shared)
	cfg.Hooks = media.Hooks{Resolver: f.res, CanUpload: f.auth, PurgePublic: func(_ context.Context, urls []string) { f.purged <- urls }}
	if mutate != nil {
		mutate(&cfg)
	}
	var err error
	if f.reg, err = media.NewRegistry(cfg); err != nil {
		t.Fatal(err)
	}
	journal, processing := env.Processing(f.reg)
	f.journal = journal
	f.q = &queue{Queue: processing, t: t, pool: env.Pool()}
	locker := s3test.Locker(t, env.Store)
	if f.jobs, err = media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: locker, Journal: journal, Processes: f.q, Pool: env.Pool()}); err != nil {
		t.Fatal(err)
	}
	f.ms = f.jobs.Manifests()
	ring, _ := token.NewRing(signKey, nil)
	if f.up, err = media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: f.ms, Tickets: &ring}); err != nil {
		t.Fatal(err)
	}
	if f.rd, err = media.NewReader(media.ReaderOptions{Manifests: f.ms, Queue: f.q,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signKey}}); err != nil {
		t.Fatal(err)
	}
	h, err := gateway.New(gateway.Config{Endpoint: env.Config.Endpoint, Bucket: env.Config.Bucket, Region: env.Config.Region,
		AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Ring: ring,
		Hosts: map[string][]string{mediaHost: f.reg.Namespaces()}})
	if err != nil {
		t.Fatal(err)
	}
	f.gateway = httptest.NewServer(h)
	t.Cleanup(f.gateway.Close)
	return f
}

// drop deletes every object under prefix (the shared namespace lives outside
// the test's tenant prefix).
func (f *fixture) drop(prefix string) {
	ctx := context.Background()
	for o, err := range f.env.Store.List(ctx, prefix) {
		if err != nil {
			return
		}
		_ = f.env.Store.Delete(ctx, o.Key)
	}
}

func (f *fixture) ref(kind string, n int) contentref.ContentRef {
	f.t.Helper()
	ref, err := f.reg.Ref(kind, cid(n))
	if err != nil {
		f.t.Fatal(err)
	}
	return ref
}

// visible makes item n visible to everyone with full access, and editable.
func (f *fixture) visible(n int) {
	f.res.set(cid(n), access.Resolution{Visible: true, Accessible: true, Editor: true})
}

// upload presigns body for path and PUTs it like the browser does,
// returning the path and name (staged upload or existing blob) to commit.
func (f *fixture) upload(ref contentref.ContentRef, path, typ string, body []byte) (string, string) {
	f.t.Helper()
	sum := sha256.Sum256(body)
	p, err := f.up.Presign(context.Background(), f.editor, media.PresignRequest{Ref: ref, Path: path, Type: typ, Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		f.t.Fatalf("presign %s: %v", path, err)
	}
	if p.Put != nil {
		req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		req.Header = p.Put.Header.Clone()
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			f.t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			f.t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, b)
		}
	}
	return p.Path, p.Blob
}

// put uploads and commits one upload.
func (f *fixture) put(ref contentref.ContentRef, path, typ string, body []byte, extra ...media.Op) *media.Manifest {
	f.t.Helper()
	p, blob := f.upload(ref, path, typ, body)
	return f.commit(ref, append([]media.Op{{Op: media.OpPut, Path: p, Blob: blob}}, extra...)...)
}

// commit commits ops, then places staged uploads as the worker does first.
func (f *fixture) commit(ref contentref.ContentRef, ops ...media.Op) *media.Manifest {
	f.t.Helper()
	m, err := f.up.Commit(context.Background(), f.editor, ref, uuid.NewString(), ops)
	if err != nil {
		f.t.Fatalf("commit %+v: %v", ops, err)
	}
	if len(m.StagedNames()) == 0 {
		return m
	}
	return f.place(ref)
}

// place stands in for the worker's place job.
func (f *fixture) place(ref contentref.ContentRef) *media.Manifest {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.ms.Place(ctx, ref); err != nil {
		f.t.Fatalf("place %s: %v", ref, err)
	}
	m, _, err := f.ms.Get(ctx, ref)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// produce stands in for the worker's producers: it records an output for
// each private preset of every upload (the upload's bytes as its blob) and
// each zip, renders its public names (the upload's bytes), and clears
// pending.
func (f *fixture) produce(ref contentref.ContentRef) {
	f.t.Helper()
	ctx := context.Background()
	item, _ := f.reg.Item(ref)
	k := item.Kind()
	m, _, err := f.ms.Get(ctx, ref)
	if err != nil {
		f.t.Fatal(err)
	}
	type public struct {
		from, blob string
		pub        media.Publication
	}
	var publics []public
	for _, u := range m.Files {
		if !u.IsUpload() || u.Blob == "" || m.Hidden {
			continue
		}
		for _, p := range k.PublicFor(u.Path) {
			names := k.PublicNames(m, p, u.Path)
			if len(names) > 0 && !u.Unattached {
				pub := media.Publication{Preset: p.Name, Source: u.Key(), FP: "test", Generation: uuid.NewString(),
					Names: names, Dims: make([]media.Dims, len(names)), State: media.PublicationReady}
				if old, ok := k.Publication(m, u, p); ok && old.Ready() {
					pub = old
				}
				for i := range pub.Dims {
					pub.Dims[i] = media.Dims{W: 1, H: 1}
				}
				publics = append(publics, public{u.Path, u.Blob, pub})
			}
		}
	}
	for _, p := range publics {
		src, _ := item.Blob(p.blob)
		for _, name := range p.pub.NamesOnDisk() {
			key, _ := item.Public(name)
			if _, err := f.env.Store.Copy(ctx, src, key, media.CopyOptions{}); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	if _, err := f.ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		for _, p := range publics {
			m.SetPublication(p.from, p.pub)
		}
		for _, u := range slicesClone(m.Files) {
			if !u.IsUpload() || u.Blob == "" {
				continue
			}
			for _, p := range k.PrivateFor(u.Path) {
				if p.Zip != "" || p.HLS != nil || p.Audio != nil {
					continue
				}
				out := media.File{Path: k.OutputPath(p, u.Path), Blob: u.Blob, Type: u.Type, Size: u.Size, FP: "test"}
				if err := m.SetOutputs(u.Path, p.Name, []media.File{out}); err != nil {
					return err
				}
			}
			for _, p := range k.PublicFor(u.Path) {
				m.ClearPending(u.Path, p.Name)
			}
		}
		for i := range k.Private {
			if z := &k.Private[i]; z.Zip != "" {
				var outs []media.File
				if in := k.ZipInputs(m, z); len(in) > 0 {
					outs = []media.File{{Path: z.To, Blob: in[0].Blob, Type: "application/zip", FP: media.ZipFP(in)}}
				}
				if err := m.SetOutputs(z.Zip, z.Name, outs); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// publicName resolves a logical preset name to the fixture's published key.
func (f *fixture) publicName(ref contentref.ContentRef, name string) string {
	f.t.Helper()
	m, _, err := f.ms.Get(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, file := range m.Files {
		for _, pub := range file.Public {
			for i, logical := range pub.Names {
				if logical == name && pub.Ready() {
					return pub.NamesOnDisk()[i]
				}
			}
		}
	}
	f.t.Fatalf("no published public image for %s", name)
	return ""
}

func slicesClone(files []media.File) []media.File { return append([]media.File(nil), files...) }

// fetch GETs a media URL through the media gateway.
func (f *fixture) fetch(u string, hdr ...string) (int, string, http.Header) {
	f.t.Helper()
	path := strings.TrimPrefix(u, "https://"+mediaHost)
	req, _ := http.NewRequest(http.MethodGet, f.gateway.URL+path, nil)
	req.Host = mediaHost
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := f.gateway.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// purges waits for the next PurgePublic call.
func (f *fixture) purges() []string {
	f.t.Helper()
	select {
	case urls := <-f.purged:
		return urls
	case <-time.After(10 * time.Second):
		f.t.Fatal("no purge")
		return nil
	}
}

func png(n int) []byte {
	return []byte(fmt.Sprintf("\x89PNG test image %d %s", n, hex.EncodeToString([]byte{byte(n)})))
}
