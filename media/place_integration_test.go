package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// Placement converges from a crash at every step, dedupes against an
// original the folder already holds, and switches every version's manifest.
func TestPlaceConvergesAndDedupes(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	s := env.Store
	r := registry(t)
	ms := s3test.Manifests(t, s, r, media.ManifestOptions{})
	work := contentref.New(env.Tenant, "gallery", uuid.Must(uuid.NewV7()).String())
	g, _ := r.Item(work)

	stage := func(body string) media.Staged {
		t.Helper()
		name := media.NewUploadName()
		key, _ := g.Original(name)
		obj, err := s.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), media.PutOptions{ContentType: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		return media.Staged{Name: name, ETag: obj.ETag, SHA256: sum[:]}
	}
	set := func(version string, files ...media.File) {
		t.Helper()
		if _, err := ms.Edit(ctx, work.WithVersion(version), func(m *media.Manifest) error { m.Files = files; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	original := func(version, file string) string {
		t.Helper()
		m, _, err := ms.Get(ctx, work.WithVersion(version))
		if err != nil {
			t.Fatal(err)
		}
		return m.Files[m.File(file)].Original
	}
	exists := func(name string) bool {
		t.Helper()
		key, _ := g.Original(name)
		_, err := s.Head(ctx, key)
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	place := func(st media.Staged) string {
		t.Helper()
		name, err := ms.Place(ctx, work, st)
		if err != nil || name != media.SHA256Name(st.SHA256) {
			t.Fatalf("place %s: %q %v", st.Name, name, err)
		}
		return name
	}

	// Two versions reference one staged upload; hls and download inputs follow the rename.
	a := stage("page a")
	set("v1", media.File{Name: "a.png", Original: a.Name, Type: "image/png",
		HLS: &media.HLS{Source: a.Name, Error: "x"}})
	set("v2", media.File{Name: "a.png", Original: a.Name, Type: "image/png"})
	if _, err := ms.Edit(ctx, work.WithVersion("v2"), func(m *media.Manifest) error {
		m.Downloads = map[string]media.Download{"a-720p": {Blob: blobName("d"), Inputs: a.Name}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	name := place(a)
	m1, _, _ := ms.Get(ctx, work.WithVersion("v1"))
	m2, _, _ := ms.Get(ctx, work.WithVersion("v2"))
	if m1.Files[0].Original != name || m1.Files[0].HLS.Source != name || m2.Files[0].Original != name || m2.Downloads["a-720p"].Inputs != name {
		t.Fatalf("references not renamed: %+v %+v", m1, m2)
	}
	if exists(a.Name) || !exists(name) {
		t.Fatal("staging kept or original missing")
	}

	// Crash after the copy: the next run renames and deletes.
	b := stage("page b")
	set("v1", media.File{Name: "b.png", Original: b.Name, Type: "image/png"})
	src, _ := g.Original(b.Name)
	dst, _ := g.Original(media.SHA256Name(b.SHA256))
	if _, err := s.Copy(ctx, src, dst, media.CopyOptions{IfMatch: b.ETag}); err != nil {
		t.Fatal(err)
	}
	place(b)
	if original("v1", "b.png") != media.SHA256Name(b.SHA256) || exists(b.Name) {
		t.Fatal("did not converge after a crash past the copy")
	}

	// Crash after deleting staging but before the second version switched.
	c := stage("page c")
	set("v1", media.File{Name: "c.png", Original: c.Name, Type: "image/png"})
	set("v2", media.File{Name: "c.png", Original: c.Name, Type: "image/png"})
	src, _ = g.Original(c.Name)
	dst, _ = g.Original(media.SHA256Name(c.SHA256))
	if _, err := s.Copy(ctx, src, dst, media.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	set("v1", media.File{Name: "c.png", Original: media.SHA256Name(c.SHA256), Type: "image/png"})
	if err := s.Delete(ctx, src); err != nil {
		t.Fatal(err)
	}
	place(c)
	if original("v2", "c.png") != media.SHA256Name(c.SHA256) {
		t.Fatal("second version not switched after a crash past the delete")
	}

	// Dedupe: the folder already holds and references these bytes, so there is no copy.
	d := stage("page a")
	set("v2", media.File{Name: "a.png", Original: name, Type: "image/png"}, media.File{Name: "again.png", Original: d.Name, Type: "image/png"})
	aKey, _ := g.Original(name)
	aBefore, _ := s.Head(ctx, aKey)
	place(d)
	aAfter, _ := s.Head(ctx, aKey)
	if !aAfter.LastModified.Equal(aBefore.LastModified) || original("v2", "again.png") != name || exists(d.Name) {
		t.Fatalf("dedupe copied or did not switch: %v → %v", aBefore.LastModified, aAfter.LastModified)
	}

	// A staged upload changed after hashing is refused; one gone with no original is typed.
	e := stage("page e")
	if _, err := ms.Place(ctx, work, media.Staged{Name: e.Name, ETag: `"0123"`, SHA256: e.SHA256}); !errors.Is(err, media.ErrPreconditionFailed) {
		t.Fatalf("changed staging: %v", err)
	}
	gone := media.Staged{Name: media.NewUploadName(), SHA256: e.SHA256}
	if _, err := ms.Place(ctx, work, gone); !errors.Is(err, media.ErrStagedGone) {
		t.Fatalf("gone staging: %v", err)
	}

	// The placed bytes are the staged ones.
	rc, _, err := s.Get(ctx, aKey, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "page a" {
		t.Fatalf("placed %q", got)
	}
}
