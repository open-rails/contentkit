package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/agent"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

const mediaHost = "media.doujins.test"

var signKey = token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

// cid is the n-th test item id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func blobOf(b []byte) string {
	sum := sha256.Sum256(b)
	return layout.SHA256Name(sum[:])
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

// queue records the processing the app asks the worker for.
type queue struct {
	mu   sync.Mutex
	jobs []media.ProcessJob
}

func (q *queue) Enqueue(_ context.Context, j media.ProcessJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	return nil
}

func (q *queue) take() []media.ProcessJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.jobs
	q.jobs = nil
	return out
}

// fixture is one test's media stack on real MinIO (and PostgreSQL for the
// manifest lock): the app's registry, uploads, reads and jobs, and the
// access agent serving its URLs.
type fixture struct {
	t      *testing.T
	env    *s3test.Env
	ns     string
	shared string
	reg    *media.Registry
	ms     *media.Manifests
	up     *media.Uploads
	rd     *media.Reader
	jobs   *media.Jobs
	res    *resolver
	auth   *authorizer
	q      *queue
	purged chan []string
	agent  *httptest.Server
	editor access.Actor
}

func newFixture(t *testing.T) *fixture {
	return newFixtureOn(t, s3test.Open(t), nil)
}

func newFixtureOn(t *testing.T, env *s3test.Env, mutate func(*media.Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, env: env, ns: env.Tenant, shared: "acct" + strings.ReplaceAll(env.Tenant, "-", ""),
		res:  &resolver{verdicts: map[string]access.Resolution{}, anon: map[string]access.Resolution{}},
		auth: &authorizer{}, q: &queue{}, purged: make(chan []string, 64), editor: access.Actor{ID: "editor", Kind: "user"}}
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
	locker := s3test.Locker(t, env.Store)
	if f.jobs, err = media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: locker, Processes: f.q, Pool: pgtest.Pool(t, nil)}); err != nil {
		t.Fatal(err)
	}
	f.ms = f.jobs.Manifests()
	ring, _ := token.NewRing(signKey, nil)
	if f.up, err = media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: f.ms, Tickets: &ring, Queue: f.q}); err != nil {
		t.Fatal(err)
	}
	if f.rd, err = media.NewReader(media.ReaderOptions{Manifests: f.ms, Queue: f.q,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signKey}}); err != nil {
		t.Fatal(err)
	}
	h, err := agent.New(agent.Config{Endpoint: env.Config.Endpoint, Bucket: env.Config.Bucket, Region: env.Config.Region,
		AccessKeyID: env.Config.AccessKeyID, SecretAccessKey: env.Config.SecretAccessKey, Ring: ring,
		Hosts: map[string][]string{mediaHost: f.reg.Namespaces()}, Defaults: media.AgentConfig(f.reg).Defaults})
	if err != nil {
		t.Fatal(err)
	}
	f.agent = httptest.NewServer(h)
	t.Cleanup(f.agent.Close)
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
	m, err := f.up.Commit(context.Background(), f.editor, ref, ops)
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
	type public struct{ key, blob string }
	var publics []public
	for _, u := range m.Files {
		if !u.IsUpload() || u.Blob == "" || m.Hidden {
			continue
		}
		for _, p := range k.PublicFor(u.Path) {
			for _, n := range k.PublicNames(m, p, u.Path) {
				key, _ := item.Public(n)
				publics = append(publics, public{key, u.Blob})
			}
		}
	}
	for _, p := range publics {
		src, _ := item.Blob(p.blob)
		if _, err := f.env.Store.Copy(ctx, src, p.key, media.CopyOptions{}); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := f.ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
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

func slicesClone(files []media.File) []media.File { return append([]media.File(nil), files...) }

// fetch GETs a media URL through the access agent.
func (f *fixture) fetch(u string, hdr ...string) (int, string, http.Header) {
	f.t.Helper()
	path := strings.TrimPrefix(u, "https://"+mediaHost)
	req, _ := http.NewRequest(http.MethodGet, f.agent.URL+path, nil)
	req.Host = mediaHost
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := f.agent.Client().Do(req)
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
