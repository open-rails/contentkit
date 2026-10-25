package critical_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

type uncertainPublicationCopy struct {
	media.Store
	mu    sync.Mutex
	fail  bool
	calls int
}

func (s *uncertainPublicationCopy) Copy(ctx context.Context, source, destination string, options media.CopyOptions) (media.Object, error) {
	object, err := s.Store.Copy(ctx, source, destination, options)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err == nil && s.fail {
		s.fail = false
		return media.Object{}, errors.New("test: copy response lost after S3 accepted it")
	}
	return object, err
}

func TestPublicationStorage(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a)
	client := a.engine.Client()
	ref := galleryRef(t, client, "Durable publication storage")
	_, err := client.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
		{Path: "cover.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
	})
	must(t, err)
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := media.NewRegistry(config)
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	journal, err := media.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	store := &uncertainPublicationCopy{Store: storage.store}
	manifests, err := media.NewManifests(store, registry, media.ManifestOptions{Locker: media.PGLocker(w.pool), Journal: journal})
	must(t, err)
	var snapshot *media.Manifest
	var original, prepared, cover string
	waitFor(t, func() bool {
		snapshot, _, err = manifests.Get(t.Context(), target)
		must(t, err)
		for _, file := range snapshot.Files {
			switch file.Path {
			case "pages/one.png":
				original = file.Blob
			case "reader/one.webp":
				prepared = file.Blob
			case "cover.png":
				if len(file.Public) > 0 && file.Public[0].Ready() && len(file.Public[0].Blobs) > 0 {
					cover = file.Public[0].Blobs[0]
				}
			}
		}
		return original != "" && prepared != "" && cover != ""
	})
	for name, blob := range map[string]string{"original": original, "page": prepared, "cover": cover} {
		area, _, valid := layout.SplitBlobRef(blob)
		want := layout.AreaFuture
		if name == "original" {
			want = layout.AreaOriginals
		}
		if !valid || area != want {
			t.Fatalf("%s preparation exposed or misplaced: %q", name, blob)
		}
	}
	ring, err := token.NewRing(storage.key, nil)
	must(t, err)
	itemToken := ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), time.Now().Add(time.Minute))
	get := func(blob, credential string, want int) []byte {
		t.Helper()
		key, err := item.Blob(blob)
		must(t, err)
		address := storage.gateway.URL + layout.URLPrefix + key
		if credential != "" {
			address += "?t=" + credential
		}
		response, err := http.Get(address)
		must(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		must(t, err)
		if response.StatusCode != want {
			t.Fatalf("%s: HTTP %d, want %d: %s", blob, response.StatusCode, want, body)
		}
		return body
	}
	get(original, itemToken, http.StatusNotFound)
	get(prepared, itemToken, http.StatusNotFound)
	get(cover, itemToken, http.StatusNotFound)

	operation := uuid.NewString()
	inputs := []media.Promotion{{Source: prepared, Area: layout.AreaPrivate}, {Source: cover, Area: layout.AreaPublic}}
	plan, err := manifests.PreparePromotion(t.Context(), target, operation, inputs)
	must(t, err)
	repeated, err := manifests.PreparePromotion(t.Context(), target, operation, inputs)
	must(t, err)
	if repeated.Files[0].Blob != plan.Files[0].Blob || repeated.Ready {
		t.Fatal("acceptance retry changed the reserved destination or falsely completed")
	}
	_, err = manifests.PreparePromotion(t.Context(), target, operation, inputs[:1])
	if !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("changed operation input was accepted: %v", err)
	}
	store.fail = true
	if _, err := manifests.Promote(t.Context(), target, operation); err == nil {
		t.Fatal("injected lost copy response did not interrupt publication")
	}
	finished, err := manifests.Promote(t.Context(), target, operation)
	must(t, err)
	if !finished.Ready || store.calls != len(inputs) {
		t.Fatalf("resume recopied a verified destination: ready=%v calls=%d", finished.Ready, store.calls)
	}
	pageBytes := get(finished.Files[0].Blob, itemToken, http.StatusOK)
	get(finished.Files[0].Blob, "", http.StatusNotFound)
	get(finished.Files[1].Blob, "", http.StatusOK)
	key, err := item.Blob(finished.Files[0].Blob)
	must(t, err)
	wrongVersion := strings.Replace(key, "/"+ref.Version+"/", "/"+uuid.NewString()+"/", 1)
	_, err = storage.store.Copy(t.Context(), key, wrongVersion, media.CopyOptions{})
	must(t, err)
	response, err := http.Get(storage.gateway.URL + layout.URLPrefix + wrongVersion + "?t=" + itemToken)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatal("one version's private token opened another version's existing object")
	}
	exact := ring.Sign(token.PrivateFileScope(key), time.Now().Add(time.Minute))
	get(finished.Files[0].Blob, exact, http.StatusOK)

	move := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), target, move, []media.Promotion{{Source: finished.Files[0].Blob, Area: layout.AreaFuture}})
	must(t, err)
	moved, err := manifests.Promote(t.Context(), target, move)
	must(t, err)
	must(t, manifests.WithdrawPromotion(t.Context(), target, operation))
	get(finished.Files[0].Blob, itemToken, http.StatusNotFound)
	get(moved.Files[0].Blob, itemToken, http.StatusNotFound)
	_, err = manifests.Promote(t.Context(), target, operation)
	if !errors.Is(err, media.ErrDeliveryWithdrawn) {
		t.Fatalf("withdrawn operation resurrected its served objects: %v", err)
	}
	republish := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), target, republish, []media.Promotion{{Source: moved.Files[0].Blob, Area: layout.AreaPrivate}})
	must(t, err)
	newRelease, err := manifests.Promote(t.Context(), target, republish)
	must(t, err)
	must(t, manifests.WithdrawPromotion(t.Context(), target, move))
	if newRelease.Files[0].Blob == finished.Files[0].Blob {
		t.Fatal("republishing reused a retired physical allocation")
	}
	if !bytes.Equal(get(newRelease.Files[0].Blob, itemToken, http.StatusOK), pageBytes) {
		t.Fatal("publication copies changed page bytes")
	}
	originalKey, err := item.Blob(original)
	must(t, err)
	_, err = storage.store.Head(t.Context(), originalKey)
	must(t, err)
}
