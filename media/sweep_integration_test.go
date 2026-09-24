package media_test

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func putObject(t *testing.T, s media.Store, key, body string) media.Object {
	t.Helper()
	obj, err := s.Put(context.Background(), key, bytes.NewReader([]byte(body)), int64(len(body)), media.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func listKeys(t *testing.T, s media.Store, prefix string) []string {
	t.Helper()
	var keys []string
	for o, err := range s.List(context.Background(), prefix) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	return keys
}

func newestModified(t *testing.T, s media.Store, prefix string) time.Time {
	t.Helper()
	var newest time.Time
	for o, err := range s.List(context.Background(), prefix) {
		if err != nil {
			t.Fatal(err)
		}
		if o.LastModified.After(newest) {
			newest = o.LastModified
		}
	}
	return newest
}

func TestSweepKeepsReferencedFreshAndSlotFiles(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	r := registry(t)
	const grace = 24 * time.Hour
	clock := time.Now()
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r, Tenants: []string{env.Tenant}, Grace: grace,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{})
	s := env.Store

	work := contentref.New(env.Tenant, "gallery", cid(1))
	g, _ := r.Item(work)
	// The host creates the item before any of its files land.
	if _, err := ms.Create(ctx, work.WithVersion("v1")); err != nil {
		t.Fatal(err)
	}
	key := func(area, name string) string { return g.Prefix() + area + "/" + name }
	upOrphan, upRef := media.NewUploadName(), media.NewUploadName()
	names := map[string]string{}
	for _, n := range []string{"origA", "origB", "origOrphan", "blobA", "blobA2", "blobB", "blobReplaced", "blobOrphan", "blobFresh"} {
		names[n] = blobName(n)
	}
	for _, n := range []string{"origA", "origB", "origOrphan"} {
		putObject(t, s, key(media.AreaOriginals, names[n]), n)
	}
	for _, n := range []string{"blobA", "blobA2", "blobB", "blobReplaced", "blobOrphan"} {
		putObject(t, s, key(media.AreaBlobs, names[n]), n)
	}
	putObject(t, s, key(media.AreaStaging, upOrphan), "abandoned multipart")
	putObject(t, s, key(media.AreaStaging, upRef), "committed multipart the worker has not placed")
	putObject(t, s, key(media.AreaOriginals, "cover"), "slot original")
	putObject(t, s, g.PublicPrefix()+"cover.webp", "public slot")
	putObject(t, s, g.Prefix()+"notes.txt", "not ours")
	// Other folders: a registered kind is swept by SweepAll, an unknown kind is not.
	post := contentref.New(env.Tenant, "post", cid(7))
	p, _ := r.Item(post)
	putObject(t, s, p.BlobsPrefix()+names["blobOrphan"], "post orphan")
	foreign := env.Tenant + "/zzz/" + cid(1) + "/blobs/" + names["blobOrphan"]
	putObject(t, s, foreign, "foreign orphan")
	user, _ := r.Item(contentref.New(env.Tenant, "user", cid(11)))
	putObject(t, s, user.OriginalsPrefix()+"avatar", "avatar original")
	putObject(t, s, user.PublicPrefix()+"avatar_80.webp", "avatar")

	variant := func(n string) map[string]media.Variant { return map[string]media.Variant{"thumb": {Blob: names[n]}} }
	for v, f := range map[string]media.File{
		"v1": {Name: "001.png", Original: names["origA"], Variants: variant("blobReplaced")},
		"v2": {Name: "001.png", Original: names["origB"], Variants: variant("blobB")},
	} {
		if _, err := ms.Edit(ctx, work.WithVersion(v), func(m *media.Manifest) error { m.Files = []media.File{f}; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ms.Edit(ctx, work.WithVersion("v2"), func(m *media.Manifest) error {
		m.Files = append(m.Files, media.File{Name: "clip.mp4", Original: upRef})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Replace v1's variant: viewers mid-stream on the old blob keep it through the grace.
	if _, err := ms.Edit(ctx, work.WithVersion("v1"), func(m *media.Manifest) error {
		m.Files[0].Variants = variant("blobA2")
		m.Downloads = map[string]media.Download{"zip": {Blob: names["blobA"]}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock = time.Now()
	res, err := jobs.Sweep(ctx, work)
	if err != nil || res.Wait <= 0 || res.Wait > grace+2*time.Second || len(res.Deleted) != 0 {
		t.Fatalf("fresh manifests must defer the sweep: %+v %v", res, err)
	}
	edited := newestModified(t, s, g.Prefix())

	time.Sleep(2 * time.Second)
	upFresh := media.NewUploadName()
	putObject(t, s, key(media.AreaBlobs, names["blobFresh"]), "job output not yet in a manifest")
	putObject(t, s, key(media.AreaStaging, upFresh), "upload not yet committed")

	clock = edited.Add(grace + time.Second)
	res, err = jobs.Sweep(ctx, work)
	if err != nil || res.Wait != 0 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	want := []string{key(media.AreaBlobs, names["blobOrphan"]), key(media.AreaBlobs, names["blobReplaced"]),
		key(media.AreaOriginals, names["origOrphan"])}
	slices.Sort(want)
	slices.Sort(res.Deleted)
	if !slices.Equal(res.Deleted, want) {
		t.Fatalf("deleted %v, want %v", res.Deleted, want)
	}
	left := listKeys(t, s, g.Prefix())
	for _, k := range []string{key(media.AreaOriginals, names["origA"]), key(media.AreaOriginals, names["origB"]),
		key(media.AreaBlobs, names["blobA"]), key(media.AreaBlobs, names["blobA2"]), key(media.AreaBlobs, names["blobB"]),
		key(media.AreaBlobs, names["blobFresh"]), key(media.AreaStaging, upFresh), key(media.AreaOriginals, "cover"),
		key(media.AreaStaging, upOrphan), key(media.AreaStaging, upRef),
		g.PublicPrefix() + "cover.webp", g.Prefix() + "notes.txt", g.ManifestsPrefix() + "v1.json", g.ManifestsPrefix() + "v2.json"} {
		if !slices.Contains(left, k) {
			t.Errorf("sweep removed %s", k)
		}
	}
	for _, k := range want {
		if slices.Contains(left, k) {
			t.Errorf("sweep kept %s", k)
		}
	}

	// The periodic pass covers every registered folder of the tenant.
	if err := jobs.SweepAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := listKeys(t, s, p.Prefix()); len(got) != 0 {
		t.Fatalf("post orphan survived the pass: %v", got)
	}
	if got := listKeys(t, s, env.Tenant+"/zzz/"); len(got) != 1 {
		t.Fatalf("unregistered kind was swept: %v", got)
	}
	if got := listKeys(t, s, user.Prefix()); len(got) != 2 {
		t.Fatalf("user slot files were swept: %v", got)
	}
	if got := listKeys(t, s, g.Prefix()); len(got) != len(left) {
		t.Fatalf("pass changed the swept folder: %v", got)
	}

	// Multipart objects may be dated at initiation: they get the 1-day abort rule on top.
	clock = clock.Add(24 * time.Hour)
	res, err = jobs.Sweep(ctx, work)
	want = []string{key(media.AreaBlobs, names["blobFresh"]), key(media.AreaStaging, upOrphan)}
	slices.Sort(res.Deleted)
	if err != nil || !slices.Equal(res.Deleted, want) {
		t.Fatalf("second sweep deleted %v (%v), want %v", res.Deleted, err, want)
	}
}
