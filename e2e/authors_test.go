package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/userinfo"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
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

// avatars is the media manifests, counting avatar queries.
type avatars struct {
	*media.Manifests
	queries atomic.Int32
}

func (a *avatars) Avatars(ctx context.Context, ids ...string) ([]media.PublicImage, error) {
	a.queries.Add(1)
	return a.Manifests.Avatars(ctx, ids...)
}

// Comment authors come from AuthKit's directory (one read per response) and
// ContentKit's own media (one avatar query): a person's own published avatar,
// the preset's default for one without, nothing for a deleted account. A
// person uploads their own avatar through the gate; another may not.
func TestAuthorsFromAuthKitAndMedia(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	alice, aliceToken := w.person(iam.Role{})
	bob, bobToken := w.person(iam.Role{})
	gone, goneToken := w.person(iam.Role{})
	dir := &directory{Lookup: w.ak.UserInfo()}
	images := &avatars{Manifests: w.jobs.Manifests()}
	f := serve(t, w.runtime(func(c *contentkit.RuntimeConfig) {
		c.UserInfo = dir
		c.Content.Media = &content.Media{Images: images, Folders: w.jobs}
	}))
	item := "/gallery/" + cid(1)

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
	published := publish(t, w, ref, plan.Path, "avatar")

	for _, token := range []string{aliceToken, bobToken, goneToken} {
		f.want(http.StatusCreated, token, "POST", item+"/comments", content.CommentInput{Body: "hi"}, nil)
	}
	if _, err := w.ak.DeleteUsers(ctx, iam.SystemIdentity(), []string{gone.ID}); err != nil {
		t.Fatal(err)
	}

	dir.gets.Store(0)
	images.queries.Store(0)
	var list []content.Comment
	f.want(http.StatusOK, "", "GET", item+"/comments", nil, &list)
	if len(list) != 3 || dir.gets.Load() != 1 || images.queries.Load() != 1 {
		t.Fatalf("%d comments, %d directory reads, %d avatar queries; want 3, 1, 1", len(list), dir.gets.Load(), images.queries.Load())
	}
	by := map[string]*content.PublicUser{}
	for _, c := range list {
		by[c.UserID] = c.Author
	}
	if a := by[alice.ID]; a == nil || a.Username != alice.Username || a.Name != alice.Username ||
		a.Avatar != published.Renditions[0].URL || a.AvatarSrcSet != published.SrcSet() {
		t.Fatalf("alice %+v, published %+v", a, published)
	}
	def := "https://media.test/v1/accounts/user/" + bob.ID + "/public/avatar-"
	if a := by[bob.ID]; a == nil || a.Username != bob.Username || a.Avatar != def+"64.webp" || !strings.HasSuffix(a.AvatarSrcSet, def+"128.webp 128w") {
		t.Fatalf("bob's default %+v", a)
	}
	if a, ok := by[gone.ID]; !ok || a != nil {
		t.Fatalf("a deleted account: %+v", a)
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
