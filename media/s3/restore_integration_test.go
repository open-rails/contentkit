package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
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
		t.Skipf("bucket versioning is %q (%v)", status, err)
	}
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	s := env.Store
	kinds, err := media.NewRegistry(
		media.Kind{Name: "channel", Slots: map[string]media.Slot{"avatar": {Aspect: media.Aspect1x1, Widths: []int{80}}}},
		media.Kind{Name: "post"})
	if err != nil {
		t.Fatal(err)
	}
	const grace = 24 * time.Hour
	clock := time.Now()
	jobs, _ := media.NewJobs(media.JobsConfig{Store: s, Kinds: kinds, Grace: grace, Now: func() time.Time { return clock }})
	ms := s3test.Manifests(t, s, kinds, media.ManifestOptions{})

	name := func(v string) string { sum := sha256.Sum256([]byte(v)); return media.SHA256Name(sum[:]) }
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
	file := func(orig, blob string) func(*media.Manifest) error {
		return func(m *media.Manifest) error {
			m.Files = []media.File{{Name: "f", Original: name(orig), Variants: map[string]media.Variant{"v": {Blob: name(blob)}}}}
			return nil
		}
	}

	channel := contentref.New(env.Tenant, "channel", cid(1))
	ch, _ := kinds.Item(channel)
	post := contentref.New(env.Tenant, "post", cid(2))
	p, _ := kinds.Item(post)
	for _, ref := range []contentref.ContentRef{channel, post} { // the host creates each item before its files land
		if _, err := ms.Create(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	put(ch.OriginalsPrefix()+name("origA"), "origA")
	put(ch.BlobsPrefix()+name("blobA"), "blobA")
	put(ch.OriginalsPrefix()+"avatar", "avatar v1")
	put(ch.PublicPrefix()+"avatar.webp", "public avatar v1")
	if _, err := ms.Edit(ctx, channel, file("origA", "blobA")); err != nil {
		t.Fatal(err)
	}
	put(p.OriginalsPrefix()+name("origP"), "origP")
	put(p.BlobsPrefix()+name("blobP"), "blobP")
	if _, err := ms.Edit(ctx, post, file("origP", "blobP")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)
	at := time.Now()
	time.Sleep(1500 * time.Millisecond)

	put(ch.OriginalsPrefix()+name("origB"), "origB")
	put(ch.BlobsPrefix()+name("blobB"), "blobB")
	if _, err := ms.Edit(ctx, channel, file("origB", "blobB")); err != nil {
		t.Fatal(err)
	}
	put(ch.OriginalsPrefix()+"avatar", "avatar v2")
	put(ch.PublicPrefix()+"avatar.webp", "public avatar v2")
	put(ch.PublicPrefix()+"banner.webp", "added after T")
	clock = time.Now().Add(grace + time.Minute)
	res, err := jobs.Sweep(ctx, channel)
	if err != nil || len(res.Deleted) != 2 {
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
	if err != nil || man.Files[0].Original != name("origA") {
		t.Fatalf("channel manifest not restored: %+v %v", man, err)
	}
	raw, _ := json.Marshal(man)
	t.Logf("restored manifest %s; report %+v", raw, rep)
	for key, want := range map[string]string{
		ch.OriginalsPrefix() + name("origA"): "origA", ch.BlobsPrefix() + name("blobA"): "blobA",
		ch.OriginalsPrefix() + "avatar": "avatar v1", ch.PublicPrefix() + "avatar.webp": "public avatar v1",
		p.OriginalsPrefix() + name("origP"): "origP", p.BlobsPrefix() + name("blobP"): "blobP",
	} {
		if got, err := read(key); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", key, got, err, want)
		}
	}
	if _, err := read(ch.PublicPrefix() + "banner.webp"); !errors.Is(err, media.ErrNotFound) {
		t.Errorf("slot added after T survived restore: %v", err)
	}
	if man, _, err := ms.Get(ctx, post); err != nil || man.Files[0].Original != name("origP") {
		t.Fatalf("deleted folder's manifest not restored: %v", err)
	}
}
