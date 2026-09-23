package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// The old race: an unreferenced original near the sweep's cutoff was reused
// by presign ("exists") and committed while the sweep, having re-checked the
// manifests just before, deleted it — leaving the manifest pointing at nothing.
// Reuse and commit now refuse such an object, so the sweep can only take
// objects no in-flight commit can newly reference.
func TestPresignAndCommitNeverReuseAnOriginalTheSweepMayTake(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	s := env.Store
	kinds, err := media.NewRegistry(media.Kind{Name: "post", Types: []string{"image/png"}, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// Grace 12s: presign reuses unreferenced objects younger than 6s; a
	// commit accepts them until 3s before the sweep may take them (age 9s).
	const grace = 12 * time.Second
	jobs, _ := media.NewJobs(media.JobsConfig{Store: s, Kinds: kinds, Grace: grace})
	manifests, _ := media.NewManifests(s, kinds, media.ManifestOptions{})
	uploads, err := media.NewUploads(media.UploadOptions{Store: s, Kinds: kinds, Manifests: manifests, Grace: grace,
		Authorizer: grants{"alice": {Allowed: true}}})
	if err != nil {
		t.Fatal(err)
	}
	alice := access.Actor{ID: "alice", Kind: "user"}
	ref := contentref.New(env.Tenant, "post", "1")
	item, _ := kinds.Item(ref)

	body := []byte("an earlier, abandoned upload")
	sum := sha256.Sum256(body)
	name := media.SHA256Name(sum[:])
	key, _ := item.Original(name)
	obj, err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/png", ChecksumSHA256: sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	if obj.LastModified.IsZero() {
		if obj, err = s.Head(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	untilAge := func(d time.Duration) { time.Sleep(time.Until(obj.LastModified.Add(d))) }
	presign := func() media.Presigned {
		t.Helper()
		p, err := uploads.Presign(ctx, alice, media.PresignRequest{Ref: ref, Type: "image/png", Size: int64(len(body)), SHA256: sum[:]})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	commit := func() error {
		_, err := uploads.Commit(ctx, alice, ref, []media.Op{{Op: media.OpInsert, Name: "a.png", Original: name}})
		return err
	}

	if p := presign(); !p.Exists {
		t.Fatal("a fresh identical upload must be reused")
	}
	untilAge(grace/2 + 500*time.Millisecond)
	p := presign()
	if p.Exists || p.Put == nil {
		t.Fatalf("an unreferenced original past grace/2 was reused: %+v", p)
	}
	// A client holding the old "exists" answer commits anyway: refused once
	// the sweep could take the object before the manifest edit lands.
	untilAge(grace - grace/4 + 500*time.Millisecond)
	var ue *media.UploadError
	if err := commit(); !errors.As(err, &ue) || ue.Code != media.CodeNotUploaded {
		t.Fatalf("commit of an original due for cleanup: %v", err)
	}
	if res, err := jobs.Sweep(ctx, ref); err != nil || len(res.Deleted) != 0 {
		t.Fatalf("sweep before the cutoff: %+v %v", res, err)
	}

	// The PUT refreshes the object; the commit then lands and protects it.
	req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
	req.Header = p.Put.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %d", resp.StatusCode)
	}
	if obj, err = s.Head(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	// Referenced, it is reused at any age and never swept.
	untilAge(grace + 2*time.Second)
	if p := presign(); !p.Exists {
		t.Fatal("a referenced original must be reused")
	}
	if res, err := jobs.Sweep(ctx, ref); err != nil || len(res.Deleted) != 0 || res.Wait != 0 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if _, err := s.Head(ctx, key); err != nil {
		t.Fatal("referenced original was swept:", err)
	}
}
