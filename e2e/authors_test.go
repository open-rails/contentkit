package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/userinfo"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/signal"
)

// directory is AuthKit's directory, counting its reads.
type directory struct {
	userinfo.Lookup
	gets atomic.Int32
}

func (d *directory) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	d.gets.Add(1)
	return d.Lookup.Get(ctx, ids)
}

// Comment authors come from AuthKit's directory (one read per response) and
// ContentKit's own media: every person's avatar at its stable URL, holding
// their published image (the gateway serves the default where none is),
// nothing for a deleted account. A person uploads their own avatar through
// the gate; another may not. Erasing a person deletes their avatar;
// erasing one without an avatar completes.
func TestAuthorsFromAuthKitAndMedia(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	alice, aliceToken := w.person(iam.Role{})
	bob, bobToken := w.person(iam.Role{})
	gone, goneToken := w.person(iam.Role{})
	dir := &directory{Lookup: w.ak.UserInfo()}
	e := w.engine(func(_ *contentkit.Config, d *contentkit.Deps) { d.Users = dir })
	f := serve(t, e)
	_, staff := w.person(w.staff)
	g, _ := w.published(f, staff, "gallery")
	item := "/contents/" + g

	// Alice uploads her avatar; Bob may not upload it for her.
	body := pngBytes(t, 32)
	sum := sha256.Sum256(body)
	presign := media.PresignBody{Ref: media.RefBody{Kind: "user", ID: alice.ID}, Path: "avatar", Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	f.want(http.StatusForbidden, bobToken, "POST", "/media/upload/presign", presign, nil)
	var plan media.PresignReply
	f.want(http.StatusOK, aliceToken, "POST", "/media/upload/presign", presign, &plan)
	req, _ := http.NewRequestWithContext(ctx, plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d %s", resp.StatusCode, b)
	}
	f.want(http.StatusOK, aliceToken, "POST", "/media/upload/commit", media.CommitBody{Ref: presign.Ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	ref, err := w.reg.Ref("user", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, w, ref, plan.Path, "avatar")

	for _, token := range []string{aliceToken, bobToken, goneToken} {
		f.want(http.StatusCreated, token, "POST", item+"/comments", content.CommentInput{Body: "hi"}, nil)
	}
	if _, err := w.ak.DeleteUsers(ctx, iam.SystemIdentity(), []string{gone.ID}); err != nil {
		t.Fatal(err)
	}

	dir.gets.Store(0)
	var list []content.Comment
	f.want(http.StatusOK, "", "GET", item+"/comments", nil, &list)
	if len(list) != 3 || dir.gets.Load() != 1 {
		t.Fatalf("%d comments, %d directory reads; want 3, 1", len(list), dir.gets.Load())
	}
	by := map[string]*content.PublicUser{}
	for _, c := range list {
		by[c.UserID] = c.Author
	}
	for _, u := range []authtest.User{alice, bob} {
		base := "https://media.test/v1/accounts/user/" + u.ID + "/public/avatar-"
		if a := by[u.ID]; a == nil || a.Username != u.Username || a.Avatar != base+"64.webp" || !strings.HasSuffix(a.AvatarSrcSet, base+"128.webp 128w") {
			t.Fatalf("%s %+v", u.Username, a)
		}
	}
	if got := stableAvatar(t, w.env.Store, alice.ID); !bytes.Equal(got, body) {
		t.Fatalf("alice's stable avatar holds %d bytes, want her %d", len(got), len(body))
	}
	if got := stableAvatar(t, w.env.Store, bob.ID); got != nil {
		t.Fatalf("bob, who uploaded none, has a stable avatar of %d bytes", len(got))
	}
	if a, ok := by[gone.ID]; !ok || a != nil {
		t.Fatalf("a deleted account: %+v", a)
	}

	if report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: alice.ID}}); err != nil || !report.Complete() {
		t.Fatalf("erasing alice: %+v %v", report, err)
	}
	// Someone who never uploaded one erases completely too, and again.
	for range 2 {
		if report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: bob.ID}}); err != nil || !report.Complete() {
			t.Fatalf("erasing bob: %+v %v", report, err)
		}
	}
	avatars, err := w.reg.Item(ref)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		left := 0
		for obj, err := range w.env.Store.List(ctx, avatars.Prefix()) {
			if err != nil {
				t.Fatal(err)
			}
			if obj.Key != avatars.ManifestKey() { // the deletion's tombstone
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alice's avatar folder keeps %d objects after her erasure", left)
		}
	}
	f.want(http.StatusOK, "", "GET", item+"/comments", nil, &list)
	for _, c := range list {
		if c.UserID == alice.ID && c.Author != nil && c.Author.Avatar != "https://media.test/v1/accounts/user/"+alice.ID+"/public/avatar-64.webp" {
			t.Fatalf("alice after erasure %+v", c.Author)
		}
	}
}

// publish stands in for the media worker: it places ref's uploads and
// renders preset from the upload at path (its bytes), as the projection then
// serves it.
func publish(t *testing.T, w *world, ref contentref.ContentRef, path, preset string) media.PublicImage {
	t.Helper()
	ctx := t.Context()
	ms := w.jobs.Manifests()
	if _, err := ms.Place(ctx, ref); err != nil {
		t.Fatal(err)
	}
	m, _, err := ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	item, err := w.reg.Item(ref)
	if err != nil {
		t.Fatal(err)
	}
	var p *media.Public
	for _, c := range item.Kind().PublicFor(path) {
		if c.Name == preset {
			p = c
		}
	}
	file, ok := m.Get(path)
	if !ok || p == nil {
		t.Fatalf("no %s upload", path)
	}
	names := item.Kind().PublicNames(m, p, path)
	pub := media.Publication{Preset: preset, Source: file.Key(), FP: "e2e", Generation: uuid.NewString(),
		Names: names, Dims: make([]media.Dims, len(names)), State: media.PublicationReady}
	src, _ := item.Blob(file.Blob)
	out := media.PublicImage{From: path, Preset: preset}
	for i, name := range pub.NamesOnDisk() {
		pub.Dims[i] = media.Dims{W: p.Widths[i], H: p.Widths[i]}
		key, _ := item.Public(name)
		if _, err := w.env.Store.Copy(ctx, src, key, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		out.Renditions = append(out.Renditions, media.PublicRendition{URL: w.reg.PublicURL(ref, name), W: p.Widths[i], H: p.Widths[i]})
	}
	if _, err := ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		m.SetPublication(path, pub)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// stableAvatar is the bytes at id's stable 64-pixel avatar name; nil when
// absent (the gateway serves the default there).
func stableAvatar(t *testing.T, store media.Store, id string) []byte {
	t.Helper()
	r, _, err := store.Get(t.Context(), "accounts/user/"+id+"/public/avatar-64.webp", media.GetOptions{})
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
