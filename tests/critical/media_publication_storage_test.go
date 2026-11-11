package critical_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

type uncertainPublicationCopy struct {
	media.Store
	mu              sync.Mutex
	fail            bool
	calls           int
	deleteFailure   string
	forbidDataReads bool
}

func (s *uncertainPublicationCopy) Get(ctx context.Context, key string, options media.GetOptions) (io.ReadCloser, media.Object, error) {
	if s.forbidDataReads && !strings.HasSuffix(key, "/manifest.json") {
		return nil, media.Object{}, errors.New("test: publication must not relay file bytes through the application")
	}
	return s.Store.Get(ctx, key, options)
}

func (s *uncertainPublicationCopy) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	fail := s.deleteFailure == key
	if fail {
		s.deleteFailure = ""
	}
	s.mu.Unlock()
	if fail {
		return errors.New("test: old served object deletion failed")
	}
	return s.Store.Delete(ctx, key)
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
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	store := &uncertainPublicationCopy{Store: storage.store}
	manifests, err := mediastore.NewManifests(store, registry, mediastore.ManifestOptions{Locker: mediastore.PGLocker(w.pool), Journal: journal})
	must(t, err)
	var snapshot *mediastore.Manifest
	var original, prepared, cover string
	waitForWithin(t, 45*time.Second, func() bool {
		snapshot, _, err = manifests.Get(t.Context(), target)
		must(t, err)
		for _, file := range snapshot.Files {
			if failure := file.Fail(); failure != nil {
				t.Fatalf("source preparation failed: %+v", failure)
			}
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
		want := layout.AreaInternal
		if name == "original" {
			want = layout.AreaOriginals
		}
		if !valid || area != want {
			t.Fatalf("%s preparation exposed or misplaced: %q", name, blob)
		}
	}
	ring, err := token.NewRing(storage.key, nil)
	must(t, err)
	itemToken := ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), time.Now().Add(5*time.Minute))
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
	store.forbidDataReads = true
	inputs := []mediastore.Promotion{{Source: prepared, Area: layout.AreaPrivate}, {Source: cover, Area: layout.AreaPublic}}
	plan, err := manifests.PreparePromotion(t.Context(), target, operation, inputs)
	must(t, err)
	repeated, err := manifests.PreparePromotion(t.Context(), target, operation, inputs)
	must(t, err)
	if repeated.Files[0].Blob != plan.Files[0].Blob || repeated.Ready {
		t.Fatal("acceptance retry changed the reserved destination or falsely completed")
	}
	_, err = manifests.PreparePromotion(t.Context(), target, operation, inputs[:1])
	if !errors.Is(err, mediastore.ErrCommitIdentity) {
		t.Fatalf("changed operation input was accepted: %v", err)
	}
	store.fail = true
	if _, err := manifests.Promote(t.Context(), target, operation); err == nil {
		t.Fatal("injected lost copy response did not interrupt publication")
	}
	preparedKey, err := item.Blob(prepared)
	must(t, err)
	store.deleteFailure = preparedKey
	if _, err := manifests.Promote(t.Context(), target, operation); err == nil {
		t.Fatal("publication completed despite failed internal-source removal")
	}
	unfinished, _, err := manifests.Get(t.Context(), target)
	must(t, err)
	for _, delivery := range unfinished.Deliveries {
		if delivery.ID == operation && (!delivery.Adopted || delivery.Ready) {
			t.Fatal("failed move did not retain adopted-but-incomplete state")
		}
	}
	finished, err := manifests.Promote(t.Context(), target, operation)
	must(t, err)
	if !finished.Ready || store.calls != len(inputs) {
		t.Fatalf("resume recopied a verified destination: ready=%v calls=%d", finished.Ready, store.calls)
	}
	if _, err := storage.store.Head(t.Context(), preparedKey); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("completed move retained an unreferenced internal source: %v", err)
	}
	unchanged := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), target, unchanged, []mediastore.Promotion{
		{Source: finished.Files[0].Blob, Area: layout.AreaPrivate},
		{Source: finished.Files[1].Blob, Area: layout.AreaPublic},
	})
	must(t, err)
	adopted, err := manifests.Promote(t.Context(), target, unchanged)
	must(t, err)
	if store.calls != len(inputs) || adopted.Files[0].Blob != finished.Files[0].Blob || adopted.Files[1].Blob != finished.Files[1].Blob {
		t.Fatal("same-area publication copied or renamed unchanged bytes")
	}
	must(t, manifests.WithdrawPromotion(t.Context(), target, operation))
	operation, finished = unchanged, adopted
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
	exact := ring.Sign(token.PrivateFileScope(key), time.Now().Add(5*time.Minute))
	get(finished.Files[0].Blob, exact, http.StatusOK)

	move := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), target, move, []mediastore.Promotion{{Source: finished.Files[0].Blob, Area: layout.AreaInternal}})
	must(t, err)
	moved, err := manifests.Promote(t.Context(), target, move)
	must(t, err)
	must(t, manifests.WithdrawPromotion(t.Context(), target, operation))
	compacted, _, err := manifests.Get(t.Context(), target)
	must(t, err)
	for _, delivery := range compacted.Deliveries {
		if delivery.Withdrawn {
			t.Fatal("completed withdrawal left historical pins in the live manifest")
		}
	}
	if _, err := manifests.PreparePromotion(t.Context(), target, operation, inputs); !errors.Is(err, mediastore.ErrDeliveryWithdrawn) {
		t.Fatalf("compacted operation was allowed to publish again: %v", err)
	}
	get(finished.Files[0].Blob, itemToken, http.StatusNotFound)
	get(moved.Files[0].Blob, itemToken, http.StatusNotFound)
	_, err = manifests.Promote(t.Context(), target, operation)
	if !errors.Is(err, mediastore.ErrDeliveryWithdrawn) {
		t.Fatalf("withdrawn operation resurrected its served objects: %v", err)
	}
	republish := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), target, republish, []mediastore.Promotion{{Source: moved.Files[0].Blob, Area: layout.AreaPrivate}})
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
	preview, err := storage.store.PresignEditorGet(t.Context(), originalKey, time.Minute)
	must(t, err)
	previewResponse, err := http.Get(preview.URL)
	must(t, err)
	previewBody, err := io.ReadAll(previewResponse.Body)
	previewResponse.Body.Close()
	must(t, err)
	if previewResponse.StatusCode != http.StatusOK || previewResponse.Header.Get("Cache-Control") != "private, no-store" || !bytes.Equal(previewBody, imageBytes(t)) {
		t.Fatal("authorized editor preview changed original bytes or allowed cache retention")
	}
	if _, err := storage.store.PresignEditorGet(t.Context(), originalKey, 6*time.Minute); err == nil {
		t.Fatal("editor preview accepted a lifetime beyond five minutes")
	}
}

// These probes wrap the real lock and S3 store to schedule an edit precisely
// between holds and count the storage work done while each hold is live.
type publicationBatchStore struct {
	media.Store
	destinations  map[string]bool
	heads, copies int
	loseCopyAt    int
	forbidHeads   bool
	panicProbe    bool
}

func (s *publicationBatchStore) Head(ctx context.Context, key string) (media.Object, error) {
	if s.destinations[key] {
		s.heads++
		if s.forbidHeads {
			return media.Object{}, errors.New("test: delivery was verified again during adoption")
		}
	}
	return s.Store.Head(ctx, key)
}

func (s *publicationBatchStore) Copy(ctx context.Context, source, destination string, options media.CopyOptions) (media.Object, error) {
	s.copies++
	object, err := s.Store.Copy(ctx, source, destination, options)
	if err == nil && s.copies == s.loseCopyAt {
		return media.Object{}, errors.New("test: copy response lost in second batch")
	}
	return object, err
}

func (s *publicationBatchStore) Capabilities(ctx context.Context) (media.Capabilities, error) {
	if s.panicProbe {
		panic("test: recovery storage probe panicked")
	}
	return s.Store.Capabilities(ctx)
}

type publicationBatchLocker struct {
	mediastore.Locker
	store      *publicationBatchStore
	after      func()
	holds      [][2]int // destination HEADs and COPYs per hold
	lastUnlock func()
}

func (l *publicationBatchLocker) Lock(ctx context.Context, key string) (func(), error) {
	unlock, err := l.Locker.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	l.lastUnlock = unlock
	heads, copies := l.store.heads, l.store.copies
	return func() {
		unlock()
		l.holds = append(l.holds, [2]int{l.store.heads - heads, l.store.copies - copies})
		if l.after != nil {
			l.after()
		}
	}, nil
}

func TestPromotionLockBatches(t *testing.T) {
	m := newResilientMedia(t)
	ctx := t.Context()
	registry := m.config.Kinds
	queue, err := workqueue.New(m.pool, registry, m.config.Schema)
	must(t, err)
	journal, err := mediastore.NewPGJournal(m.pool, m.schema, queue)
	must(t, err)
	clean, err := mediastore.NewManifests(m.storage.store, registry, mediastore.ManifestOptions{Locker: mediastore.PGLocker(m.pool), Journal: journal})
	must(t, err)
	for _, scenario := range []string{"commits_and_derivative_edits", "withdrawal", "retirement", "source_retired_at_adoption", "changed_delivery_at_adoption", "partial_copy_retry", "panic_in_complete_commit"} {
		t.Run(scenario, func(t *testing.T) {
			ref := galleryRef(t, m.app.engine.Client(), scenario)
			target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
			must(t, err)
			item, err := registry.Item(target)
			must(t, err)
			payload := []byte("immutable publication output")
			sum := sha256.Sum256(payload)
			sums := make([][]byte, 33)
			for i := range sums {
				sums[i] = sum[:]
			}
			blobs, err := clean.NewBlobs(ctx, target, layout.AreaInternal, sums)
			must(t, err)
			inputs := make([]mediastore.Promotion, len(blobs))
			for i, blob := range blobs {
				key, err := item.Blob(blob)
				must(t, err)
				_, err = m.storage.store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), media.PutOptions{ContentType: "image/webp", ChecksumSHA256: sum[:]})
				must(t, err)
				inputs[i] = mediastore.Promotion{Source: blob, Area: layout.AreaPublic}
			}
			_, err = clean.EditExisting(ctx, target, func(current *mediastore.Manifest) error {
				for i, blob := range blobs {
					current.Files = append(current.Files, media.File{Path: fmt.Sprintf("reader/%02d.webp", i), Blob: blob, Type: "image/webp", Size: int64(len(payload)), Preset: "reader", From: "pages/one.png", FP: "initial"})
				}
				return nil
			})
			must(t, err)
			operation := uuid.NewString()
			plan, err := clean.PreparePromotion(ctx, target, operation, inputs)
			must(t, err)
			store := &publicationBatchStore{Store: m.storage.store, destinations: map[string]bool{}}
			for _, file := range plan.Files {
				key, err := item.Blob(file.Blob)
				must(t, err)
				store.destinations[key] = true
			}
			locker := &publicationBatchLocker{Locker: mediastore.PGLocker(m.pool), store: store}
			manifests, err := mediastore.NewManifests(store, registry, mediastore.ManifestOptions{Locker: locker, Journal: journal})
			must(t, err)
			edits := 0
			locker.after = func() {
				if store.heads == 0 {
					return
				}
				if store.copies > 16 && len(locker.holds) == 1 {
					t.Errorf("first hold copied %d files, want at most 16", store.copies)
				}
				switch scenario {
				case "commits_and_derivative_edits":
					if store.copies == len(blobs) {
						return
					}
					_, err := m.app.engine.Client().PostMediaUploadCommit(ctx, media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMeta, Meta: map[string]any{"between_batches": store.copies}}}})
					must(t, err)
					_, err = clean.EditExisting(ctx, target, func(current *mediastore.Manifest) error { current.Files[0].FP = fmt.Sprint(store.copies); return nil })
					must(t, err)
					edits++
				case "withdrawal":
					locker.after = nil
					must(t, clean.WithdrawPromotion(ctx, target, operation))
				case "retirement":
					locker.after = nil
					key, err := item.Blob(plan.Files[16].Blob)
					must(t, err)
					_, err = m.pool.Exec(ctx, `UPDATE `+pgx.Identifier{m.schema, "content_media_allocations"}.Sanitize()+` SET retired_at=now() WHERE tenant_id=$1 AND object_key=$2`, target.TenantID, key)
					must(t, err)
				case "source_retired_at_adoption":
					if store.copies != len(blobs) {
						return
					}
					locker.after = nil
					key, err := item.Blob(plan.Files[0].Source)
					must(t, err)
					_, err = m.pool.Exec(ctx, `UPDATE `+pgx.Identifier{m.schema, "content_media_allocations"}.Sanitize()+` SET retired_at=now() WHERE tenant_id=$1 AND object_key=$2`, target.TenantID, key)
					must(t, err)
				case "changed_delivery_at_adoption":
					if store.copies != len(blobs) {
						return
					}
					locker.after = nil
					_, err := clean.EditExisting(ctx, target, func(current *mediastore.Manifest) error {
						for i := range current.Deliveries {
							if current.Deliveries[i].ID == operation {
								files := current.Deliveries[i].Files
								files[0], files[1] = files[1], files[0]
							}
						}
						return nil
					})
					must(t, err)
					store.forbidHeads = true
				default:
					locker.after = nil
				}
			}
			if scenario == "panic_in_complete_commit" {
				locker.after = nil
				store.panicProbe = true
				func() {
					defer func() {
						if recover() == nil {
							t.Error("recovery did not panic")
						}
					}()
					_ = manifests.CompleteCommit(ctx, target, uuid.NewString())
				}()
				// Clean up even the unchanged implementation's leaked holder.
				defer locker.lastUnlock()
				release, ok, err := pglock.Acquire(ctx, m.pool, "contentkit:media:"+item.ManifestKey(), false)
				must(t, err)
				if !ok {
					t.Fatal("CompleteCommit retained its manifest lock after panic")
				}
				release()
				return
			}
			if scenario == "partial_copy_retry" {
				store.loseCopyAt = 18
			}
			finished, err := manifests.Promote(ctx, target, operation)
			switch scenario {
			case "withdrawal":
				if !errors.Is(err, mediastore.ErrDeliveryWithdrawn) || store.copies != 16 {
					t.Fatalf("withdrawal: copies=%d err=%v", store.copies, err)
				}
			case "retirement":
				if !errors.Is(err, mediastore.ErrAllocationRetired) || store.copies != 16 {
					t.Fatalf("retirement: copies=%d err=%v", store.copies, err)
				}
			case "source_retired_at_adoption":
				if !errors.Is(err, mediastore.ErrAllocationRetired) {
					t.Fatalf("adoption accepted a retired source pin: %v", err)
				}
			case "changed_delivery_at_adoption":
				if !errors.Is(err, mediastore.ErrCommitIdentity) {
					t.Fatalf("changed delivery reused stale proof: %v", err)
				}
			case "partial_copy_retry":
				if err == nil || store.copies != 18 {
					t.Fatalf("partial copy: copies=%d err=%v", store.copies, err)
				}
				finished, err = manifests.Promote(ctx, target, operation)
				must(t, err)
				if !finished.Ready || store.copies != len(blobs) || !slices.Equal(finished.Files, plan.Files) {
					t.Fatalf("retry changed or recopied immutable destinations: copies=%d", store.copies)
				}
			default:
				must(t, err)
				if !finished.Ready || edits != 2 {
					t.Fatalf("publication did not allow edits between batches: ready=%v edits=%d", finished.Ready, edits)
				}
				current, _, err := clean.Get(ctx, target)
				must(t, err)
				if current.Meta["between_batches"] != float64(32) || current.Files[0].FP != "32" {
					t.Fatal("adoption lost concurrent commit or derivative edit")
				}
				if store.heads != 2*len(blobs) {
					t.Fatalf("adoption re-HEADed immutable delivery: got %d destination HEADs", store.heads)
				}
			}
			for _, hold := range locker.holds {
				if hold[0] > 32 || hold[1] > 16 {
					t.Errorf("unbounded hold: %d destination HEADs, %d copies", hold[0], hold[1])
				}
			}
		})
	}
}
