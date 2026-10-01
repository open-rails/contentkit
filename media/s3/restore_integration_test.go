package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

func TestRestoreAfterSweepAndFolderDeletion(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	if env.Created {
		if err := env.Store.Configure(ctx, 30); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := env.VersioningStatus(ctx); err != nil || status != types.BucketVersioningStatusEnabled {
		t.Fatalf("bucket versioning is %q (%v), want enabled", status, err)
	}
	s := env.Store
	images := []string{"image/png"}
	kinds, err := media.NewRegistry(media.Config{Namespace: env.Tenant, Kinds: []media.Kind{
		{Name: "channel", KeepOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "v", From: "files/{name}", To: "v/{name}.webp", Image: &media.Image{}}}},
		{Name: "post", KeepOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "v", From: "files/{name}", To: "v/{name}.webp", Image: &media.Image{}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	const grace = 24 * time.Hour
	clock := time.Now()
	jobs, err := media.NewJobs(media.JobsConfig{Store: s, Locker: s3test.Locker(t, s), Registry: kinds, Grace: grace, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	ms := jobs.Manifests()

	name := func(v string) string { sum := sha256.Sum256([]byte(v)); return layout.SHA256Name(sum[:]) }
	put := func(key, body string) {
		if _, err := s.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(key string) (string, error) {
		rc, _, err := s.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}
	// file is a manifest of one upload and its output.
	file := func(orig, out string) func(*media.Manifest) error {
		return func(m *media.Manifest) error {
			m.Files = []media.File{{Path: "files/f.png", Blob: name(orig), Type: "image/png"},
				{Path: "v/f.webp", Blob: name(out), Type: "image/webp", From: "files/f.png", Preset: "v", FP: "x"}}
			return nil
		}
	}
	channel := contentref.New(env.Tenant, "channel", cid(1))
	ch, _ := kinds.Item(channel)
	post := contentref.New(env.Tenant, "post", cid(2))
	p, _ := kinds.Item(post)
	blob := func(i media.Item, v string) string { k, _ := i.Blob(name(v)); return k }

	for _, ref := range []contentref.ContentRef{channel, post} { // the host creates each item before its files land
		if _, err := ms.Create(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	put(blob(ch, "origA"), "origA")
	put(blob(ch, "outA"), "outA")
	if _, err := ms.Edit(ctx, channel, file("origA", "outA")); err != nil {
		t.Fatal(err)
	}
	put(blob(p, "origP"), "origP")
	put(blob(p, "outP"), "outP")
	if _, err := ms.Edit(ctx, post, file("origP", "outP")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)
	at := time.Now()
	time.Sleep(1500 * time.Millisecond)

	put(blob(ch, "origB"), "origB")
	put(blob(ch, "outB"), "outB")
	if _, err := ms.Edit(ctx, channel, file("origB", "outB")); err != nil {
		t.Fatal(err)
	}
	clock = time.Now().Add(grace + time.Minute)
	res, err := jobs.Sweep(ctx, channel)
	if err != nil || len(res.Deleted) != 2 { // A's upload and output
		t.Fatalf("sweep: %+v %v", res, err)
	}
	for o, err := range s.List(ctx, p.Prefix()) { // an erased item: its folder deleted
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := s.Restore(ctx, env.Tenant+"/", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 0 {
		t.Fatalf("missing after restore: %v", rep.Missing)
	}
	man, _, err := ms.Get(ctx, channel)
	if err != nil || man.Files[0].Blob != name("origA") {
		t.Fatalf("channel manifest not restored: %+v %v", man, err)
	}
	for key, want := range map[string]string{
		blob(ch, "origA"): "origA", blob(ch, "outA"): "outA", blob(p, "origP"): "origP", blob(p, "outP"): "outP",
	} {
		if got, err := read(key); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", key, got, err, want)
		}
	}
	if man, _, err := ms.Get(ctx, post); err != nil || man.Files[0].Blob != name("origP") {
		t.Fatalf("deleted folder's manifest not restored: %v", err)
	}
}
