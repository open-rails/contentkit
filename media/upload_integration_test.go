package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
)

// grants is the host permission check: actor id → grant. "translator" may
// upload to versions only; "owner2" to content id 2 only.
type grants map[string]media.UploadGrant

func (g grants) CanUpload(_ context.Context, a access.Actor, ref contentref.ContentRef) (media.UploadGrant, error) {
	switch a.ID {
	case "translator":
		return media.UploadGrant{Allowed: ref.Version() != ""}, nil
	case "owner2":
		return media.UploadGrant{Allowed: ref.ContentID == "2"}, nil
	}
	return g[a.ID], nil
}

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

func (q *queue) count() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.jobs) }

type uploadEnv struct {
	*s3test.Env
	store     media.Store
	uploads   *media.Uploads
	manifests *media.Manifests
	queue     *queue
	srv       *httptest.Server
}

func newUploadEnv(t *testing.T, caps *media.Capabilities, limiter media.UploadLimiter) *uploadEnv {
	t.Helper()
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	var store media.Store = env.Store
	if caps != nil {
		store = env.WithCapabilities(t, *caps)
	}
	kinds, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
			Slots: map[string]media.Slot{"cover": {Aspect: media.Aspect3x1, Widths: []int{460}}}},
		media.Kind{Name: "video", Types: []string{"video/mp4"}, MaxBytes: 1 << 30},
		media.Kind{Name: "post", Types: []string{"image/png"}, MaxBytes: 1 << 20, Inline: &media.Spec{Width: 1600}},
		media.Kind{Name: "mixed", Versioned: true, Types: []string{"image/png", "video/mp4"}, MaxBytes: 1 << 20, MaxFiles: 3,
			TypeLimits: map[string]media.Limit{"video": {MaxBytes: 1 << 30, MaxFiles: 1}}, Video: &media.Video{},
			Slots: map[string]media.Slot{"cover": {Aspect: media.Ratio("1:2"), Widths: []int{100}}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	manifests := s3test.Manifests(t, store, kinds, media.ManifestOptions{})
	ring, err := token.NewRing(token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := &queue{}
	u, err := media.NewUploads(media.UploadOptions{Store: store, Kinds: kinds, Manifests: manifests, Tickets: &ring, Limiter: limiter, Queue: q,
		Authorizer: grants{
			"alice":  {Allowed: true, Owner: "chan-a"},
			"bob":    {Allowed: true, Owner: "chan-a"},
			"admin":  {Allowed: true, Exempt: true, Owner: "chan-a"},
			"reader": {Allowed: false},
		}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: manifests, Kinds: kinds, Resolver: &resolver{},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: slotBase, SigningKey: token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	h := media.UploadHandler(u, media.UploadHandlerOptions{Tenant: env.Tenant, Reader: reader, Actor: func(r *http.Request) (access.Actor, bool) {
		id := r.Header.Get("X-Test-Actor")
		return access.Actor{ID: id, Kind: "user"}, id != ""
	}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &uploadEnv{Env: env, store: store, uploads: u, manifests: manifests, queue: q, srv: srv}
}

const slotBase = "https://media.example"

// call posts body as actor and decodes a 2xx reply into out, or returns the error reply.
func (e *uploadEnv) call(t *testing.T, actor, path string, body, out any) (int, media.ErrorReply) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader(b))
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var er media.ErrorReply
	if resp.StatusCode >= 300 {
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	} else if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, er
}

func (e *uploadEnv) presign(t *testing.T, actor string, ref media.RefBody, typ string, body []byte) (int, media.PresignReply, media.ErrorReply) {
	t.Helper()
	var out media.PresignReply
	status, er := e.call(t, actor, "/presign", media.PresignBody{Ref: ref, Type: typ, Size: int64(len(body)), SHA256: hexSum(body)}, &out)
	return status, out, er
}

// upload presigns and PUTs body, returning its original name.
func (e *uploadEnv) upload(t *testing.T, actor string, ref media.RefBody, typ string, body []byte) string {
	t.Helper()
	status, p, er := e.presign(t, actor, ref, typ, body)
	if status != http.StatusOK {
		t.Fatalf("presign: %d %+v", status, er)
	}
	if p.Put != nil {
		if code := put(t, p.Put, body, nil); code != http.StatusOK {
			t.Fatalf("put: %d", code)
		}
	}
	return p.Name
}

func (e *uploadEnv) commit(t *testing.T, actor string, ref media.RefBody, ops ...media.Op) (int, media.CommitReply, media.ErrorReply) {
	t.Helper()
	var out media.CommitReply
	status, er := e.call(t, actor, "/commit", media.CommitBody{Ref: ref, Ops: ops}, &out)
	return status, out, er
}

func put(t *testing.T, r *media.RequestReply, body []byte, override map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(r.Method, r.URL, bytes.NewReader(body))
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range override {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func data(seed uint64, n int) []byte {
	r := rand.New(rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)}))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func insert(name, original string) media.Op {
	return media.Op{Op: media.OpInsert, Name: name, Original: original}
}

func TestSingleUploadBindingsAndCommit(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "gallery", ID: "1", Version: "en"}
	body := data(1, 4096)

	for name, tc := range map[string]struct {
		actor string
		body  media.PresignBody
		code  string
		want  int
	}{
		"over cap":        {"alice", media.PresignBody{Ref: ref, Type: "image/png", Size: 11 << 20, SHA256: hexSum(body)}, media.CodeTooLarge, 413},
		"disallowed type": {"alice", media.PresignBody{Ref: ref, Type: "image/gif", Size: 10, SHA256: hexSum(body)}, media.CodeType, 415},
		"missing hash":    {"alice", media.PresignBody{Ref: ref, Type: "image/png", Size: 10}, media.CodeInvalid, 400},
		"unknown kind":    {"alice", media.PresignBody{Ref: media.RefBody{Kind: "nope", ID: "1"}, Type: "image/png", Size: 10, SHA256: hexSum(body)}, media.CodeNotFound, 404},
		"not allowed":     {"reader", media.PresignBody{Ref: ref, Type: "image/png", Size: 10, SHA256: hexSum(body)}, media.CodeForbidden, 403},
		"anonymous":       {"", media.PresignBody{Ref: ref, Type: "image/png", Size: 10, SHA256: hexSum(body)}, "unauthorized", 401},
	} {
		status, er := e.call(t, tc.actor, "/presign", tc.body, nil)
		if status != tc.want || er.Code != tc.code {
			t.Errorf("%s: %d %+v", name, status, er)
		}
	}

	status, p, er := e.presign(t, "alice", ref, "image/png", body)
	if status != 200 || p.Put == nil || p.Name != "sha256-"+hexSum(body) {
		t.Fatalf("presign %d %+v %+v", status, p, er)
	}
	wrong := bytes.Clone(body)
	wrong[0] ^= 0xff
	if code := put(t, p.Put, wrong, nil); code < 400 && e.store.Capabilities().ChecksumSHA256 {
		t.Fatalf("wrong bytes accepted: %d", code)
	}
	if code := put(t, p.Put, body, map[string]string{"Content-Type": "image/jpeg"}); code != 403 {
		t.Fatalf("wrong type: %d", code)
	}
	if code := put(t, p.Put, body[:4000], nil); code != 403 {
		t.Fatalf("wrong length: %d", code)
	}
	if code := put(t, p.Put, body, nil); code != 200 {
		t.Fatalf("exact put: %d", code)
	}

	// A commit before the object lands, or naming another item's upload, finds nothing.
	other := e.upload(t, "alice", media.RefBody{Kind: "gallery", ID: "2", Version: "en"}, "image/png", data(2, 100))
	if status, _, er := e.commit(t, "alice", ref, insert("x.png", other)); status != 409 || er.Code != media.CodeNotUploaded || len(er.Originals) != 1 || er.Originals[0] != other {
		t.Fatalf("foreign original: %d %+v", status, er)
	}
	if status, _, er := e.commit(t, "reader", ref, insert("001.png", p.Name)); status != 403 {
		t.Fatalf("commit without permission: %d %+v", status, er)
	}

	status, c, er := e.commit(t, "alice", ref, insert("001.png", p.Name))
	if status != 200 || len(c.Files) != 1 || c.Files[0].Size != 4096 || c.Files[0].Type != "image/png" {
		t.Fatalf("commit %d %+v %+v", status, c, er)
	}
	// Retrying the same insert is a no-op; the same name with other bytes conflicts.
	if status, c, _ := e.commit(t, "alice", ref, insert("001.png", p.Name)); status != 200 || len(c.Files) != 1 {
		t.Fatalf("retried commit %d %+v", status, c)
	}
	second := e.upload(t, "alice", ref, "image/jpeg", data(3, 2048))
	if status, _, er := e.commit(t, "alice", ref, insert("001.png", second)); status != 409 || er.Code != media.CodeConflict {
		t.Fatalf("duplicate name: %d %+v", status, er)
	}

	// An identical file already in the folder needs no upload.
	if status, p, _ := e.presign(t, "bob", ref, "image/png", body); status != 200 || !p.Exists || p.Put != nil {
		t.Fatalf("existing presign %+v", p)
	}

	zero := 0
	status, c, er = e.commit(t, "alice", ref,
		insert("000.jpg", second),
		media.Op{Op: media.OpMove, Name: "000.jpg", Index: &zero},
		media.Op{Op: media.OpRename, Name: "001.png", To: "page-1.png"},
	)
	if status != 200 || len(c.Files) != 2 || c.Files[0].Name != "000.jpg" || c.Files[1].Name != "page-1.png" {
		t.Fatalf("ops %d %+v %+v", status, c, er)
	}
	third := e.upload(t, "alice", ref, "image/png", data(4, 3000))
	status, c, _ = e.commit(t, "alice", ref,
		media.Op{Op: media.OpReplace, Name: "page-1.png", Original: third},
		media.Op{Op: media.OpRemove, Name: "000.jpg"})
	if status != 200 || len(c.Files) != 1 || c.Files[0].Original != third || c.Files[0].Size != 3000 {
		t.Fatalf("replace/remove %d %+v", status, c)
	}
	if status, _, er := e.commit(t, "alice", ref, media.Op{Op: media.OpRemove, Name: "missing"}); status != 404 {
		t.Fatalf("remove missing: %d %+v", status, er)
	}
	if e.queue.count() != 4 {
		t.Fatalf("%d processing jobs, want one per commit", e.queue.count())
	}
	man, _, err := e.manifests.Get(ctx, contentref.NewVersion(e.Tenant, "gallery", "1", "en"))
	if err != nil || len(man.Files) != 1 || man.Files[0].Name != "page-1.png" {
		t.Fatalf("manifest %+v %v", man, err)
	}
}

func TestCommitRehashesWithoutChecksumEnforcement(t *testing.T) {
	e := newUploadEnv(t, &media.Capabilities{ConditionalPut: true}, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "post", ID: "7"}
	item, _ := media.NewRegistry(media.Kind{Name: "post"})
	it, _ := item.Item(contentref.New(e.Tenant, "post", "7"))

	good := data(5, 5000)
	name := e.upload(t, "alice", ref, "image/png", good)
	if status, _, er := e.commit(t, "alice", ref, insert("ok.png", name)); status != 200 {
		t.Fatalf("re-hashed good upload: %d %+v", status, er)
	}

	// A backend that ignores the signed checksum stores other bytes under the name.
	claimed := data(6, 5000)
	liar := "sha256-" + hexSum(claimed)
	key, _ := it.Original(liar)
	if _, err := e.Store.Put(ctx, key, bytes.NewReader(good), int64(len(good)), media.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if status, _, er := e.commit(t, "alice", ref, insert("bad.png", liar)); status != 422 || er.Code != media.CodeChecksum {
		t.Fatalf("mismatch: %d %+v", status, er)
	}
	if _, err := e.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("mismatching object kept: %v", err)
	}
}

func TestSlotUploadAndCommit(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "gallery", ID: "9"}
	cover := data(7, 1234)
	var p media.PresignReply
	if status, er := e.call(t, "alice", "/presign", media.PresignBody{Ref: ref, Type: "image/png", Size: 1234, SHA256: hexSum(cover), Slot: "cover"}, &p); status != 200 {
		t.Fatalf("slot presign %d %+v", status, er)
	}
	if !strings.HasSuffix(p.Put.URL[:strings.IndexByte(p.Put.URL, '?')], "/gallery/9/originals/cover") {
		t.Fatalf("slot url %s", p.Put.URL)
	}
	if status, _ := e.call(t, "alice", "/presign", media.PresignBody{Ref: ref, Type: "image/png", Size: 1, SHA256: hexSum(cover), Slot: "banner"}, nil); status != 404 {
		t.Fatalf("unknown slot: %d", status)
	}
	if status, er := e.call(t, "alice", "/commit-slot", media.SlotBody{Ref: ref, Slot: "cover", SHA256: hexSum(cover)}, nil); status != 409 || er.Code != media.CodeNotUploaded {
		t.Fatalf("commit before upload: %d %+v", status, er)
	}
	if code := put(t, p.Put, cover, nil); code != 200 {
		t.Fatalf("slot put %d", code)
	}
	crop := func(x, y, w, h int) *media.Edit { return &media.Edit{Crop: &media.Crop{X: x, Y: y, W: w, H: h}} }
	for _, bad := range []*media.Edit{crop(-1, 0, 600, 0), crop(0, 0, 0, 0), {Rotate: 45}} {
		if status, er := e.call(t, "alice", "/commit-slot", media.SlotBody{Ref: ref, Slot: "cover", SHA256: hexSum(cover), Edit: bad}, nil); status != 400 || er.Code != media.CodeInvalid {
			t.Fatalf("edit %+v: %d %+v", bad, status, er)
		}
	}
	// The crop's height follows its width at the slot's 3:1 aspect.
	var m media.SlotManifest
	if status, er := e.call(t, "alice", "/commit-slot", media.SlotBody{Ref: ref, Slot: "cover", SHA256: hexSum(cover), Edit: crop(10, 20, 600, 7)}, &m); status != 200 {
		t.Fatalf("commit slot %d %+v", status, er)
	}
	if !m.Pending || m.Edit == nil || *m.Edit.Crop != (media.Crop{X: 10, Y: 20, W: 600, H: 200}) || m.Aspect != media.Aspect3x1 || len(m.Outputs) != 0 {
		t.Fatalf("manifest after commit %+v", m)
	}
	if e.queue.count() != 1 || e.queue.jobs[0].Slot != "cover" {
		t.Fatalf("jobs %+v", e.queue.jobs)
	}
	obj, err := e.Store.Head(ctx, e.Tenant+"/gallery/9/originals/cover")
	if err != nil || obj.Size != 1234 {
		t.Fatalf("slot original %+v %v", obj, err)
	}

	// The editor reads the committed original back; others may not.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/slot-original", strings.NewReader(`{"ref":{"kind":"gallery","id":"9"},"slot":"cover"}`))
	req.Header.Set("X-Test-Actor", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, cover) || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("slot original: %d %q", resp.StatusCode, resp.Header)
	}
	if status, er := e.call(t, "reader", "/slot-original", media.SlotRefBody{Ref: ref, Slot: "cover"}, nil); status != 403 {
		t.Fatalf("reader read the original: %d %+v", status, er)
	}

	// Edit-slot reuses the original; no edit centres.
	m = media.SlotManifest{}
	if status, er := e.call(t, "alice", "/edit-slot", media.SlotEditBody{Ref: ref, Slot: "cover"}, &m); status != 200 || m.Edit != nil || !m.Pending {
		t.Fatalf("edit-slot %d %+v %+v", status, er, m)
	}
	if e.queue.count() != 2 {
		t.Fatalf("edit-slot enqueued %d", e.queue.count())
	}
	if status, er := e.call(t, "reader", "/edit-slot", media.SlotEditBody{Ref: ref, Slot: "cover", Edit: crop(0, 0, 300, 0)}, nil); status != 403 {
		t.Fatalf("reader edited: %d %+v", status, er)
	}
	if status, er := e.call(t, "alice", "/edit-slot", media.SlotEditBody{Ref: media.RefBody{Kind: "gallery", ID: "10"}, Slot: "cover"}, nil); status != 404 {
		t.Fatalf("edit of an uncommitted slot: %d %+v", status, er)
	}
	// A new upload not committed yet cannot be edited or read back.
	next := data(8, 999)
	var p2 media.PresignReply
	if status, er := e.call(t, "alice", "/presign", media.PresignBody{Ref: ref, Type: "image/png", Size: 999, SHA256: hexSum(next), Slot: "cover"}, &p2); status != 200 {
		t.Fatalf("presign %d %+v", status, er)
	}
	if code := put(t, p2.Put, next, nil); code != 200 {
		t.Fatalf("put %d", code)
	}
	if status, er := e.call(t, "alice", "/edit-slot", media.SlotEditBody{Ref: ref, Slot: "cover"}, nil); status != 409 || er.Code != media.CodeNotUploaded {
		t.Fatalf("edit of a replaced original: %d %+v", status, er)
	}
	if status, er := e.call(t, "alice", "/slot-original", media.SlotRefBody{Ref: ref, Slot: "cover"}, nil); status != 409 {
		t.Fatalf("read of a replaced original: %d %+v", status, er)
	}
	if status, er := e.call(t, "alice", "/slot", media.SlotRefBody{Ref: ref, Slot: "nope"}, nil); status != 404 {
		t.Fatalf("unknown slot manifest: %d %+v", status, er)
	}
	rec, err := e.manifests.Slot(ctx, contentref.New(e.Tenant, "gallery", "9"), "cover")
	if err != nil || rec.Original != obj.ETag || rec.Edit != nil {
		t.Fatalf("record %+v %v", rec, err)
	}
}

func TestInlineUploadAndCommit(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "post", ID: "p1"}
	img := data(8, 2048)
	body := media.PresignBody{Ref: ref, Type: "image/png", Size: 2048, SHA256: hexSum(img), Inline: true}
	var p, again media.PresignReply
	if status, er := e.call(t, "alice", "/presign", body, &p); status != 200 || !strings.HasPrefix(p.Name, "i-") || p.Put == nil {
		t.Fatalf("inline presign %d %+v %+v", status, er, p)
	}
	if e.call(t, "alice", "/presign", body, &again); again.Name == p.Name {
		t.Fatal("inline names must be fresh")
	}
	for _, bad := range []media.PresignBody{
		{Ref: media.RefBody{Kind: "gallery", ID: "9"}, Type: "image/png", Size: 2048, SHA256: hexSum(img), Inline: true},
		{Ref: ref, Type: "image/png", Size: 2048, SHA256: hexSum(img), Slot: p.Name}, // no overwriting an inline image
	} {
		if status, _ := e.call(t, "alice", "/presign", bad, nil); status < 400 {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if code := put(t, p.Put, img, nil); code != 200 {
		t.Fatalf("inline put %d", code)
	}
	if status, er := e.call(t, "alice", "/commit-slot", media.SlotBody{Ref: ref, Slot: p.Name, SHA256: hexSum(img)}, nil); status != 204 {
		t.Fatalf("commit inline %d %+v", status, er)
	}
	if e.queue.count() != 1 || e.queue.jobs[0].Slot != p.Name {
		t.Fatalf("jobs %+v", e.queue.jobs)
	}
	if obj, err := e.Store.Head(ctx, e.Tenant+"/post/p1/originals/"+p.Name); err != nil || obj.Size != 2048 {
		t.Fatalf("inline original %+v %v", obj, err)
	}
}

// failingReader delivers n bytes and then fails, killing the request mid-body.
type failingReader struct {
	r io.Reader
	n int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection killed")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

func TestMultipartResumeAfterKilledPart(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	ref := media.RefBody{Kind: "video", ID: "88"}
	body := data(8, media.MaxSinglePut+(1<<20)+123)
	var p media.PresignReply
	if status, er := e.call(t, "alice", "/presign", media.PresignBody{Ref: ref, Type: "video/mp4", Size: int64(len(body))}, &p); status != 200 || p.Multipart == nil {
		t.Fatalf("presign %d %+v", status, er)
	}
	if !strings.HasPrefix(p.Name, "u-") || p.Multipart.MaxParts != 9 {
		t.Fatalf("plan %+v %+v", p, p.Multipart)
	}
	ticket := p.Multipart.Ticket

	// Adaptive sizes: two 8 MiB parts, then 16 MiB parts, then the remainder.
	var sizes []int
	for off, i := 0, 0; off < len(body); i++ {
		n := media.MaxPartSize
		if i < 2 {
			n = media.MinPartSize
		}
		n = min(n, len(body)-off)
		sizes = append(sizes, n)
		off += n
	}
	chunk := func(n int32) []byte {
		off := 0
		for i := range int(n) - 1 {
			off += sizes[i]
		}
		return body[off : off+sizes[n-1]]
	}
	sign := func(numbers ...int32) map[int32]*media.RequestReply {
		var req media.PartsBody
		req.Ticket = ticket
		for _, n := range numbers {
			req.Parts = append(req.Parts, media.PartBody{Number: n, Size: int64(sizes[n-1]), SHA256: hexSum(chunk(n))})
		}
		var out media.PartsReply
		if status, er := e.call(t, "alice", "/parts", req, &out); status != 200 {
			t.Fatalf("sign parts: %d %+v", status, er)
		}
		m := map[int32]*media.RequestReply{}
		for _, p := range out.Parts {
			m[p.Number] = p.Request
		}
		return m
	}

	for name, bad := range map[string]media.PartsBody{
		"oversized part": {Ticket: ticket, Parts: []media.PartBody{{Number: 1, Size: media.MaxPartSize + 1, SHA256: hexSum(nil)}}},
		"too many parts": {Ticket: ticket, Parts: []media.PartBody{{Number: 10, Size: 1, SHA256: hexSum(nil)}}},
		"forged ticket":  {Ticket: "x" + ticket, Parts: []media.PartBody{{Number: 1, Size: 1, SHA256: hexSum(nil)}}},
	} {
		if status, er := e.call(t, "alice", "/parts", bad, nil); status != 400 {
			t.Errorf("%s: %d %+v", name, status, er)
		}
	}
	if status, er := e.call(t, "bob", "/parts", media.PartsBody{Ticket: ticket, Parts: []media.PartBody{{Number: 1, Size: 1, SHA256: hexSum(nil)}}}, nil); status != 403 {
		t.Fatalf("another uploader's ticket: %d %+v", status, er)
	}

	urls := sign(1, 2, 3)
	for _, n := range []int32{1, 2} {
		if code := put(t, urls[n], chunk(n), nil); code != 200 {
			t.Fatalf("part %d: %d", n, code)
		}
	}
	// Part 3 dies halfway through its body.
	req, _ := http.NewRequest(urls[3].Method, urls[3].URL, &failingReader{r: bytes.NewReader(chunk(3)), n: sizes[2] / 2})
	req.ContentLength = int64(sizes[2])
	for k, v := range urls[3].Headers {
		req.Header.Set(k, v)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("killed part answered %d", resp.StatusCode)
	}

	// Resume: the server lists what landed; completing early keeps the upload.
	var listed media.PartsReply
	if status, _ := e.call(t, "alice", "/parts/list", media.TicketBody{Ticket: ticket}, &listed); status != 200 {
		t.Fatalf("list: %d", status)
	}
	if len(listed.Parts) != 2 || listed.Parts[1].Size != int64(sizes[1]) {
		t.Fatalf("listed %+v", listed.Parts)
	}
	if status, er := e.call(t, "alice", "/complete", media.TicketBody{Ticket: ticket}, nil); status != 409 || er.Code != media.CodeIncomplete {
		t.Fatalf("early complete: %d %+v", status, er)
	}
	rest := []int32{}
	for n := int32(3); int(n) <= len(sizes); n++ {
		rest = append(rest, n)
	}
	urls = sign(rest...)
	for _, n := range rest {
		if code := put(t, urls[n], chunk(n), nil); code != 200 {
			t.Fatalf("part %d: %d", n, code)
		}
	}
	var done media.CompleteReply
	if status, er := e.call(t, "alice", "/complete", media.TicketBody{Ticket: ticket}, &done); status != 200 || done.Size != int64(len(body)) || done.Name != p.Name {
		t.Fatalf("complete: %d %+v %+v", status, er, done)
	}
	// A retried complete (lost response) answers the same.
	if status, _ := e.call(t, "alice", "/complete", media.TicketBody{Ticket: ticket}, &done); status != 200 {
		t.Fatalf("retried complete: %d", status)
	}
	status, c, er := e.commit(t, "alice", ref, insert("source", p.Name))
	if status != 200 || c.Files[0].Size != int64(len(body)) || c.Files[0].Type != "video/mp4" {
		t.Fatalf("commit %d %+v %+v", status, c, er)
	}
	stagedKey := e.Tenant + "/video/88/staging/" + p.Name
	rc, staged, err := e.Store.Get(ctx, stagedKey, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatal("assembled object differs from the upload")
	}

	// The worker places the staged upload under the SHA-256 it hashed; a rerun converges.
	sum := sha256.Sum256(got)
	cref := contentref.New(e.Tenant, "video", "88")
	for range 2 {
		name, err := e.manifests.Place(ctx, cref, media.Staged{Name: p.Name, ETag: staged.ETag, SHA256: sum[:]})
		if err != nil || name != media.SHA256Name(sum[:]) {
			t.Fatalf("place: %q %v", name, err)
		}
	}
	man, _, err := e.manifests.Get(ctx, cref)
	if err != nil || man.Files[0].Original != media.SHA256Name(sum[:]) {
		t.Fatalf("manifest not switched: %+v %v", man, err)
	}
	if _, err := e.Store.Head(ctx, stagedKey); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("staging kept: %v", err)
	}
	placed, err := e.Store.Head(ctx, e.Tenant+"/video/88/originals/"+media.SHA256Name(sum[:]))
	if err != nil || placed.Size != int64(len(body)) || placed.ContentType != "video/mp4" {
		t.Fatalf("placed original %+v %v", placed, err)
	}
}

func TestMultipartOverDeclaredSizeIsAborted(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ref := media.RefBody{Kind: "video", ID: "89"}
	size := media.MaxSinglePut + 1
	var p media.PresignReply
	if status, _ := e.call(t, "alice", "/presign", media.PresignBody{Ref: ref, Type: "video/mp4", Size: int64(size)}, &p); status != 200 {
		t.Fatal(status)
	}
	// Nine full 8 MiB parts overshoot the declared size.
	part := data(9, media.MinPartSize)
	var numbers []media.PartBody
	for n := int32(1); n <= 9; n++ {
		numbers = append(numbers, media.PartBody{Number: n, Size: int64(len(part)), SHA256: hexSum(part)})
	}
	var out media.PartsReply
	if status, er := e.call(t, "alice", "/parts", media.PartsBody{Ticket: p.Multipart.Ticket, Parts: numbers}, &out); status != 200 {
		t.Fatalf("%d %+v", status, er)
	}
	for _, pr := range out.Parts {
		if code := put(t, pr.Request, part, nil); code != 200 {
			t.Fatalf("part %d: %d", pr.Number, code)
		}
	}
	if status, er := e.call(t, "alice", "/complete", media.TicketBody{Ticket: p.Multipart.Ticket}, nil); status != 400 {
		t.Fatalf("overshoot: %d %+v", status, er)
	}
	if status, _ := e.call(t, "alice", "/parts/list", media.TicketBody{Ticket: p.Multipart.Ticket}, nil); status != 404 {
		t.Fatalf("overshooting upload not aborted: %d", status)
	}
}

func TestConcurrentCommitsToOneManifest(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ref := media.RefBody{Kind: "gallery", ID: "3", Version: "ja"}
	const n = 8
	names := make([]string, n)
	for i := range n {
		names[i] = e.upload(t, "alice", ref, "image/png", data(uint64(100+i), 1000+i))
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, _, er := e.commit(t, []string{"alice", "bob"}[i%2], ref, insert(fmt.Sprintf("%03d.png", i), names[i])); status != 200 {
				t.Errorf("commit %d: %d %+v", i, status, er)
			}
		}()
	}
	wg.Wait()
	man, _, err := e.manifests.Get(context.Background(), contentref.NewVersion(e.Tenant, "gallery", "3", "ja"))
	if err != nil || len(man.Files) != n {
		t.Fatalf("%d of %d commits landed: %v", len(man.Files), n, err)
	}
}

func TestUploadLimiter(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	ctx := context.Background()
	schema := pgtest.Schema(t, ctx, pool)
	limiter, err := media.NewPGLimiter(pool, schema, media.PGLimits{FilesPerHour: 6, BytesPerDay: 40000,
		Quota: func(context.Context, string, string) (int64, error) { return 10000, nil }})
	if err != nil {
		t.Fatal(err)
	}
	e := newUploadEnv(t, nil, limiter)
	ref := media.RefBody{Kind: "post", ID: "501"}
	usage := func() (int64, int64) {
		used, pending, err := limiter.Usage(ctx, e.Tenant, "chan-a")
		if err != nil {
			t.Fatal(err)
		}
		return used, pending
	}

	// Concurrent presigns cannot exceed the owner's quota: 3 × 3000 fit in 10000.
	files := make([][]byte, 8)
	codes := make([]int, 8)
	replies := make([]media.PresignReply, 8)
	var wg sync.WaitGroup
	for i := range files {
		files[i] = data(uint64(200+i), 3000)
		wg.Add(1)
		go func() {
			defer wg.Done()
			var er media.ErrorReply
			codes[i], replies[i], er = e.presign(t, []string{"alice", "bob"}[i%2], ref, "image/png", files[i])
			if codes[i] != 200 && (codes[i] != 413 || er.Code != media.CodeQuota) {
				t.Errorf("presign %d: %d %+v", i, codes[i], er)
			}
		}()
	}
	wg.Wait()
	var ok []int
	for i, c := range codes {
		if c == 200 {
			ok = append(ok, i)
		}
	}
	if len(ok) != 3 {
		t.Fatalf("%d presigns within a 10000-byte quota, want 3", len(ok))
	}
	if used, pending := usage(); used != 0 || pending != 9000 {
		t.Fatalf("usage %d/%d", used, pending)
	}

	// Commit settles to the stored size; removal releases it.
	first := ok[0]
	if code := put(t, replies[first].Put, files[first], nil); code != 200 {
		t.Fatal(code)
	}
	if status, _, er := e.commit(t, "alice", ref, insert("a.png", replies[first].Name)); status != 200 {
		t.Fatalf("commit %d %+v", status, er)
	}
	if used, pending := usage(); used != 3000 || pending != 6000 {
		t.Fatalf("after commit %d/%d", used, pending)
	}
	if status, _, _ := e.commit(t, "alice", ref, media.Op{Op: media.OpRemove, Name: "a.png"}); status != 200 {
		t.Fatal(status)
	}
	if used, _ := usage(); used != 0 {
		t.Fatalf("after remove %d", used)
	}

	// Rate: quota refusals rolled back, so alice has one file per successful presign
	// this hour. Her next 400-byte presigns fit the quota until the hourly limit.
	alice := 0
	for _, i := range ok {
		if i%2 == 0 {
			alice++
		}
	}
	for i := range 7 - alice {
		status, _, er := e.presign(t, "alice", ref, "image/png", data(uint64(301+i), 400))
		if i < 6-alice && status != 200 {
			t.Fatalf("presign %d: %d %+v", i, status, er)
		}
		if i == 6-alice && (status != 429 || er.Code != media.CodeRate) {
			t.Fatalf("presign %d over the hourly limit: %d %+v", i, status, er)
		}
	}
	var keys []string
	for obj, err := range e.Store.List(ctx, e.Tenant+"/post/501/originals/") {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) != 1 {
		t.Fatalf("only the one uploaded file may exist: %v", keys)
	}
	e2 := data(300, 400)
	// The exempt role bypasses rate and quota.
	if status, _, er := e.presign(t, "admin", ref, "image/png", e2); status != 200 {
		t.Fatalf("exempt: %d %+v", status, er)
	}
}

func TestUploadLimiterBytesPerDayAndExpiry(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	ctx := context.Background()
	schema := pgtest.Schema(t, ctx, pool)
	limiter, err := media.NewPGLimiter(pool, schema, media.PGLimits{BytesPerDay: 10000, ReservationTTL: 1e9,
		Quota: func(context.Context, string, string) (int64, error) { return 7000, nil }})
	if err != nil {
		t.Fatal(err)
	}
	res := func(uploader, key string, size int64) error {
		return limiter.Reserve(ctx, media.Reservation{Tenant: "t", Uploader: uploader, Owner: "o", Key: key, Size: size})
	}
	if err := res("u1", "k1", 6000); err != nil {
		t.Fatal(err)
	}
	var ue *media.UploadError
	if err := res("u2", "k2", 2000); !errors.As(err, &ue) || ue.Code != media.CodeQuota {
		t.Fatalf("quota: %v", err)
	}
	// Re-presigning the same key replaces its reservation instead of adding to it.
	if err := res("u2", "k1", 6500); err != nil {
		t.Fatalf("re-reserve: %v", err)
	}
	// Abandoned reservations expire.
	deadline := func() bool {
		_, pending, err := limiter.Usage(ctx, "t", "o")
		return err == nil && pending == 0
	}
	for i := 0; !deadline(); i++ {
		if i > 50 {
			t.Fatal("reservation never expired")
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := res("u2", "k3", 2000); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	// u1 has 6000 of 10000 bytes today.
	if err := res("u1", "k4", 5000); !errors.As(err, &ue) || ue.Code != media.CodeRate || ue.RetryAfter <= 0 {
		t.Fatalf("bytes/day: %v", err)
	}
	// Settle drops reservations and moves usage, never below zero.
	if err := limiter.Settle(ctx, media.Settlement{Tenant: "t", Owner: "o", Keys: []string{"k3"}, Delta: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Settle(ctx, media.Settlement{Tenant: "t", Owner: "o", Delta: -5000}); err != nil {
		t.Fatal(err)
	}
	if used, pending, _ := limiter.Usage(ctx, "t", "o"); used != 0 || pending != 0 {
		t.Fatalf("usage %d/%d", used, pending)
	}
}

func TestSlotWritesAuthorizeTheWork(t *testing.T) {
	e := newUploadEnv(t, nil, nil)
	ctx := context.Background()
	version := media.RefBody{Kind: "gallery", ID: "9", Version: "en"}
	cover := data(9, 1500)

	// A version uploader may commit pages but not write the work's slot.
	page := e.upload(t, "translator", version, "image/png", cover)
	if status, _, er := e.commit(t, "translator", version, insert("001.png", page)); status != 200 {
		t.Fatalf("version commit %d %+v", status, er)
	}
	if status, er := e.call(t, "translator", "/presign", media.PresignBody{Ref: version, Type: "image/png", Size: 1500, SHA256: hexSum(cover), Slot: "cover"}, nil); status != 403 {
		t.Fatalf("slot presign through a version: %d %+v", status, er)
	}
	var p media.PresignReply
	if status, er := e.call(t, "alice", "/presign", media.PresignBody{Ref: version, Type: "image/png", Size: 1500, SHA256: hexSum(cover), Slot: "cover"}, &p); status != 200 {
		t.Fatalf("slot presign %d %+v", status, er)
	}
	if code := put(t, p.Put, cover, nil); code != 200 {
		t.Fatal(code)
	}
	if status, er := e.call(t, "translator", "/commit-slot", media.SlotBody{Ref: version, Slot: "cover", SHA256: hexSum(cover)}, nil); status != 403 {
		t.Fatalf("slot commit through a version: %d %+v", status, er)
	}
	if status, er := e.call(t, "translator", "/commit-slot-from-file", media.SlotFromFileBody{Ref: version, Slot: "cover", File: "001.png"}, nil); status != 403 {
		t.Fatalf("slot from a version file: %d %+v", status, er)
	}
	if status, er := e.call(t, "translator", "/presign", media.PresignBody{Ref: media.RefBody{Kind: "post", ID: "p9"}, Type: "image/png", Size: 1500, SHA256: hexSum(cover), Inline: true}, nil); status != 403 {
		t.Fatalf("inline presign: %d %+v", status, er)
	}
	if status, er := e.call(t, "alice", "/commit-slot-from-file", media.SlotFromFileBody{Ref: version, Slot: "cover", File: "001.png"}, nil); status != 200 {
		t.Fatalf("slot from file: %d %+v", status, er)
	}

	// From another item: both items must allow the actor.
	other := media.RefBody{Kind: "gallery", ID: "2"}
	from := media.SlotFromFileBody{Ref: other, Slot: "cover", From: &version, File: "001.png"}
	if status, er := e.call(t, "owner2", "/commit-slot-from-file", from, nil); status != 403 {
		t.Fatalf("slot from an unauthorized item: %d %+v", status, er)
	}
	if status, er := e.call(t, "alice", "/commit-slot-from-file", from, nil); status != 200 {
		t.Fatalf("slot from another item: %d %+v", status, er)
	}
	if obj, err := e.Store.Head(ctx, e.Tenant+"/gallery/2/originals/cover"); err != nil || obj.Size != 1500 {
		t.Fatalf("copied slot %+v %v", obj, err)
	}
	last := e.queue.jobs[e.queue.count()-1]
	if last.Slot != "cover" || last.Ref.ContentID != "2" || last.Ref.Version() != "" {
		t.Fatalf("job %+v", last)
	}
}

func TestCommitEnforcesQuota(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	ctx := context.Background()
	schema := pgtest.Schema(t, ctx, pool)
	// Reservations lapse at once: commit alone must hold the quota.
	limiter, err := media.NewPGLimiter(pool, schema, media.PGLimits{ReservationTTL: time.Microsecond,
		Quota: func(context.Context, string, string) (int64, error) { return 5000, nil }})
	if err != nil {
		t.Fatal(err)
	}
	e := newUploadEnv(t, nil, limiter)
	ref := media.RefBody{Kind: "post", ID: "q1"}
	a, b, c := data(401, 3000), data(402, 3000), data(403, 1000)
	na, nb, nc := e.upload(t, "alice", ref, "image/png", a), e.upload(t, "alice", ref, "image/png", b), e.upload(t, "alice", ref, "image/png", c)
	if status, _, er := e.commit(t, "alice", ref, insert("a.png", na)); status != 200 {
		t.Fatalf("commit a %d %+v", status, er)
	}
	if status, _, er := e.commit(t, "bob", ref, insert("b.png", nb)); status != 413 || er.Code != media.CodeQuota {
		t.Fatalf("commit over quota: %d %+v", status, er)
	}
	// Swapping within the quota nets out; a retried insert is free.
	if status, _, er := e.commit(t, "alice", ref, media.Op{Op: media.OpRemove, Name: "a.png"}, insert("b.png", nb), insert("c.png", nc)); status != 200 {
		t.Fatalf("swap %d %+v", status, er)
	}
	if status, _, er := e.commit(t, "alice", ref, insert("c.png", nc)); status != 200 {
		t.Fatalf("retry %d %+v", status, er)
	}
	if used, _, err := limiter.Usage(ctx, e.Tenant, "chan-a"); err != nil || used != 4000 {
		t.Fatalf("used %d %v", used, err)
	}
	man, _, err := e.manifests.Get(ctx, contentref.New(e.Tenant, "post", "q1"))
	if err != nil || len(man.Files) != 2 || man.OriginalBytes() != 4000 {
		t.Fatalf("manifest %+v %v", man, err)
	}
	// Exempt grants are charged but not refused.
	if status, _, er := e.commit(t, "admin", ref, insert("a.png", na)); status != 200 {
		t.Fatalf("exempt %d %+v", status, er)
	}
	if used, _, _ := limiter.Usage(ctx, e.Tenant, "chan-a"); used != 7000 {
		t.Fatalf("used %d after exempt commit", used)
	}
}
