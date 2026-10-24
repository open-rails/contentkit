package media_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media/token"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

type eraseStore struct {
	media.Store
	fail atomic.Bool
}

func (s *eraseStore) Delete(ctx context.Context, key string) error {
	if s.fail.Load() {
		return errors.New("injected storage cleanup failure")
	}
	return s.Store.Delete(ctx, key)
}

func TestAccountErasureWaitsForVerifiedStorageCleanup(t *testing.T) {
	scope := layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()}
	f := scopedFixture(t, s3test.Open(t), scope)
	var otherURLs []struct {
		f   *fixture
		url string
	}
	for _, owner := range []layout.Scope{{TenantID: scope.TenantID, AppID: uuid.NewString()}, {TenantID: uuid.NewString(), AppID: scope.AppID}} {
		other := scopedFixture(t, f.env, owner)
		other.visible(1)
		ref := other.ref("gallery", 1)
		other.put(ref, "cover", "image/png", png(3))
		other.produce(ref)
		otherURLs = append(otherURLs, struct {
			f   *fixture
			url string
		}{other, other.reg.PublicURL(ref, other.publicName(ref, "cover-230.webp"))})
	}
	ref := f.ref("user", 1)
	f.visible(1)
	f.put(ref, "avatar", "image/png", png(1))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	private, _ := item.Blob(f.fileBlob(ref, "avatar.png"))
	ring, _ := token.NewRing(signKey, nil)
	oldURL := "https://" + mediaHost + layout.URLPrefix + private + "?t=" + ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), time.Now().Add(time.Hour))
	if code, _, _ := f.fetch(oldURL); code != http.StatusOK {
		t.Fatalf("pre-erasure token: %d", code)
	}
	store := &eraseStore{Store: f.env.Store}
	store.fail.Store(true)
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	hostSchema(t, jobs, f.env.Pool())
	deletion := media.Deletion{Ref: ref, Owner: ref.ContentID, OperationID: "account-erasure"}
	if err = jobs.Erase(t.Context(), deletion); err == nil {
		t.Fatal("failed deletion reported complete")
	}
	if code, _, _ := f.fetch(oldURL); code != 404 {
		t.Fatalf("failed cleanup left old token usable: %d", code)
	}
	store.fail.Store(false)
	if err = jobs.Erase(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	for obj, err := range f.env.Store.List(t.Context(), item.Prefix()) {
		if err != nil || obj.Key != item.ManifestKey() && obj.Key != item.PublicPrefix()+layout.ErasureFenceName {
			t.Fatalf("erasure left %s %v", obj.Key, err)
		}
	}
	// An upload ticket issued before erasure may still land; retry and the
	// durable late pass use its already-retired exact allocation key.
	if _, err = f.env.Store.Put(t.Context(), private, strings.NewReader("late"), 4, media.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.fetch(oldURL); code != 404 {
		t.Fatalf("late upload escaped durable fence: %d", code)
	}
	if err = jobs.Erase(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	for _, other := range otherURLs {
		if code, _, _ := other.f.fetch(other.url); code != 200 {
			t.Fatalf("erasure reached another app/tenant root: %d", code)
		}
	}
	if _, err = f.env.Store.Head(t.Context(), private); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("late bytes survived: %v", err)
	}
}
