package image_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// A staged (multipart) image is hashed from the bytes read for decoding and
// placed at its content address in the same pass; a second staged copy of
// bytes the folder already holds is deduplicated.
func TestStagedImageIsPlacedWhileDerived(t *testing.T) {
	e := newEnv(t, galleryKind())
	ctx := context.Background()
	ref := contentref.NewVersion(e.Tenant, "gallery", uuid.Must(uuid.NewV7()).String(), "en")
	item, _ := e.kinds.Item(ref)
	body := pngImage(t, 640, 960, 5)
	sum := sha256.Sum256(body)
	placed := media.SHA256Name(sum[:])

	stage := func() string {
		t.Helper()
		name := media.NewUploadName()
		key, _ := item.Original(name)
		if _, err := e.Env.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
		return name
	}
	first := stage()
	e.commit(t, ref, ins("001.png", first))
	e.drain(t)
	m, _ := e.manifest(t, ref)
	if f := m.Files[0]; f.Original != placed || len(f.Variants) != 3 || f.Dims == nil {
		t.Fatalf("not placed and derived in one run: %+v", f)
	}
	if key, _ := item.Original(first); e.exists(t, key) {
		t.Fatal("staging kept after placement")
	}

	second := stage()
	e.commit(t, ref, ins("002.png", second))
	e.drain(t)
	m, _ = e.manifest(t, ref)
	if f := m.Files[1]; f.Original != placed || len(f.Variants) != 3 {
		t.Fatalf("duplicate not deduplicated: %+v", f)
	}
	if key, _ := item.Original(second); e.exists(t, key) {
		t.Fatal("duplicate staging kept")
	}
}

func (e *env) exists(t *testing.T, key string) bool {
	t.Helper()
	_, err := e.Env.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}
