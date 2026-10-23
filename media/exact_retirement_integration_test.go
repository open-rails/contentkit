package media_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

type noDiscoveryStore struct {
	media.Store
	lists      atomic.Int32
	failDelete atomic.Bool
}

func (s *noDiscoveryStore) List(context.Context, string) iter.Seq2[media.Object, error] {
	s.lists.Add(1)
	return func(yield func(media.Object, error) bool) {
		yield(media.Object{}, errors.New("unexpected bucket discovery"))
	}
}
func (s *noDiscoveryStore) Delete(ctx context.Context, key string) error {
	if s.failDelete.Load() {
		return errors.New("temporary DELETE failure")
	}
	return s.Store.Delete(ctx, key)
}

func exactManifests(t *testing.T, f *fixture) (*media.Manifests, *noDiscoveryStore) {
	t.Helper()
	store := &noDiscoveryStore{Store: f.env.Store}
	m, err := media.NewManifests(store, f.reg, media.ManifestOptions{Locker: s3test.Locker(t, f.env.Store), Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func TestExactRetirementPreservesSharedSourcesAndNeedsNoList(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	old := f.fileBlob(ref, "originals/001.png")
	item, _ := f.reg.Item(ref)
	key, _ := item.Blob(old)
	m, store := exactManifests(t, f)
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		cp := m.Files[0]
		cp.Path = "originals/002.png"
		m.Files = append(m.Files, cp)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		m.Files = slices.DeleteFunc(m.Files, func(f media.File) bool { return f.Path == "originals/001.png" })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !f.exists(key) {
		t.Fatal("shared allocation deleted")
	}
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error { m.Files = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if f.exists(key) {
		t.Fatal("last reference did not delete allocation")
	}
	if store.lists.Load() != 0 {
		t.Fatal("manifest edit listed storage")
	}
}

func TestExactRetirementDeleteFailureReplaysRecordedKeys(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	item, _ := f.reg.Item(ref)
	key, _ := item.Blob(f.fileBlob(ref, "originals/001.png"))
	m, store := exactManifests(t, f)
	store.failDelete.Store(true)
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error { m.Files = nil; return nil }); err == nil {
		t.Fatal("expected DELETE failure")
	}
	if !f.exists(key) {
		t.Fatal("source disappeared despite failed DELETE")
	}
	store.failDelete.Store(false)
	if err := m.Recover(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if f.exists(key) {
		t.Fatal("recovery did not delete recorded source")
	}
	if store.lists.Load() != 0 {
		t.Fatal("recovery listed storage")
	}
}

func TestExactRetirementBarrierFailureKeepsBytesUntilRecovery(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panics], func(t *testing.T) {
			var fail atomic.Bool
			f := newFixtureOn(t, s3test.Open(t), func(cfg *media.Config) {
				cfg.Hooks.BeforeRetire = func(context.Context, contentref.ContentRef, *media.Manifest) error {
					if fail.Load() {
						if panics {
							panic("barrier")
						}
						return errors.New("barrier")
					}
					return nil
				}
			})
			f.visible(1)
			ref := f.ref("gallery", 1)
			f.put(ref, "originals/001.png", "image/png", png(1))
			item, _ := f.reg.Item(ref)
			key, _ := item.Blob(f.fileBlob(ref, "originals/001.png"))
			fail.Store(true)
			if _, err := f.ms.EditExisting(t.Context(), ref, func(m *media.Manifest) error { m.Files = nil; return nil }); err == nil {
				t.Fatal("barrier failure did not stop deletion")
			}
			if !f.exists(key) {
				t.Fatal("barrier failed after deletion")
			}
			fail.Store(false)
			if err := f.ms.Recover(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if f.exists(key) {
				t.Fatal("barrier retry did not complete deletion")
			}
		})
	}
}

func TestExactCleanupUsesOnlyRecordedAllocations(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	item, _ := f.reg.Item(ref)
	stray := item.PrivatePrefix() + "unknown-integrity-error"
	f.object(stray, "unknown")
	allocation := f.blob(ref, []byte("abandoned output"), "image/png")
	key, _ := item.Blob(allocation)
	store := &noDiscoveryStore{Store: f.env.Store}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Now: func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := jobs.Sweep(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Deleted, key) || f.exists(key) {
		t.Fatal("known abandoned allocation survived")
	}
	if !f.exists(stray) || store.lists.Load() != 0 {
		t.Fatal("cleanup discovered or removed unknown object")
	}
}

func TestExactTemporaryReservationAndExpiry(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	item, _ := f.reg.Item(ref)
	name := "zip-cache-" + uuid.NewString()
	key := item.TempPrefix() + name
	if err := f.ms.ReserveTemporary(t.Context(), ref, name); err != nil {
		t.Fatal(err)
	}
	if err := f.ms.ReserveTemporary(t.Context(), ref, name); err != nil {
		t.Fatal("reservation retry", err)
	}
	var count int
	table := pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize()
	if err := f.env.Pool().QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE object_key=$1 AND retired_at IS NULL", key).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unrecorded cache: %d %v", count, err)
	}
	f.object(key, "cache")
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Now: func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Sweep(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if f.exists(key) {
		t.Fatal("expired cache survived")
	}
	if err := f.ms.ReserveTemporary(t.Context(), ref, name); !errors.Is(err, media.ErrAllocationRetired) {
		t.Fatal("retired cache was revived", err)
	}
}

func TestExactRetirementLostManifestResponse(t *testing.T) {
	for _, land := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-written", true: "written"}[land], func(t *testing.T) {
			f := newFixture(t)
			f.visible(1)
			ref := f.ref("gallery", 1)
			f.put(ref, "originals/001.png", "image/png", png(1))
			item, _ := f.reg.Item(ref)
			key, _ := item.Blob(f.fileBlob(ref, "originals/001.png"))
			lost := errors.New("lost PUT response")
			store := &lossyStore{Store: f.env.Store, key: item.ManifestKey(), land: land, err: lost}
			m := s3test.Manifests(t, store, f.reg, media.ManifestOptions{Journal: f.journal})
			if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error { m.Files = nil; return nil }); !errors.Is(err, lost) {
				t.Fatal(err)
			}
			if !f.exists(key) {
				t.Fatal("ambiguous write deleted bytes before recovery")
			}
			if err := m.Recover(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			if f.exists(key) == land {
				t.Fatalf("land=%v source-present=%v", land, f.exists(key))
			}
		})
	}
}

func TestExactTemporaryExpiryRemovesStaleManifestUpload(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	name, staged := f.upload(ref, "originals/pending.png", "image/png", png(1))
	if _, err := f.up.Commit(t.Context(), f.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: name, Blob: staged}}); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(ref)
	key, _ := item.Staged(staged)
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Now: func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Sweep(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.StagedNames()) != 0 || f.exists(key) {
		t.Fatal("expired staged reference or object survived")
	}
}

func TestExactRetirementKeepsExcludedOriginalAndReordering(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	f.put(ref, "originals/002.png", "image/png", png(2))
	item, _ := f.reg.Item(ref)
	first, _ := item.Blob(f.fileBlob(ref, "originals/001.png"))
	second, _ := item.Blob(f.fileBlob(ref, "originals/002.png"))
	m, store := exactManifests(t, f)
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		m.Files[0].Meta = map[string]any{"page": map[string]any{"mode": "exclude", "included": false}}
		slices.Reverse(m.Files)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !f.exists(first) || !f.exists(second) || store.lists.Load() != 0 {
		t.Fatal("metadata-only editing deleted an original")
	}
}

func TestExactRetirementLateReplayDoesNotRestoreOldMirror(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(cfg *media.Config) {
		for i := range cfg.Kinds {
			if cfg.Kinds[i].Name == "user" {
				cfg.Kinds[i].Public[0].Stable = true
			}
		}
	})
	f.visible(1)
	ref := f.ref("user", 1)
	f.put(ref, "avatar.png", "image/png", png(1))
	f.produce(ref)
	f.put(ref, "avatar.png", "image/png", png(2))
	f.produce(ref)
	f.put(ref, "avatar.png", "image/png", png(3))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	key, _ := item.Public("avatar-64.webp")
	before, err := f.env.Store.Head(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.later().CleanupDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	after, err := f.env.Store.Head(t.Context(), key)
	if err != nil || after.ETag != before.ETag {
		t.Fatalf("late cleanup replay changed current stable image: before=%s after=%s err=%v", before.ETag, after.ETag, err)
	}
}

func TestExactRetirementPurgeRelayFailureRecoversSameKeys(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	var calls [][]string
	fail := true
	m := f.ms.WithPublicPurge(func(_ context.Context, keys []string) error {
		calls = append(calls, slices.Clone(keys))
		if fail {
			return errors.New("queue unavailable")
		}
		return nil
	})
	if _, err := m.EditExisting(t.Context(), ref, func(m *media.Manifest) error { m.Files = nil; return nil }); err == nil {
		t.Fatal("failed purge relay did not retain pending commit")
	}
	if len(calls) != 1 || len(calls[0]) == 0 {
		t.Fatal("retired public keys not relayed")
	}
	fail = false
	if err := m.Recover(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !slices.Equal(calls[0], calls[1]) {
		t.Fatalf("recovery changed exact purge keys: %+v", calls)
	}
}

func TestPublicOnlyRetirementRecoversLateWriteWithoutDiscovery(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	old, _ := item.Public(f.publicName(ref, "cover-230.webp"))
	if _, err := f.republish(f.ms, ref, "cover.png", "cover"); err != nil {
		t.Fatal(err)
	}
	current, _ := item.Public(f.publicName(ref, "cover-230.webp"))
	if old == current || f.exists(old) {
		t.Fatal("public generation was not retired inline")
	}
	f.object(old, "late public PUT after the producer crashed")
	store := &noDiscoveryStore{Store: f.env.Store}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Now: func() time.Time { return time.Now().Add(10 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := jobs.CleanupDue(t.Context(), 100); err != nil || n == 0 {
		t.Fatalf("late public cleanup: %d %v", n, err)
	}
	if f.exists(old) || !f.exists(current) || store.lists.Load() != 0 {
		t.Fatal("late cleanup used discovery or deleted the current generation")
	}
}

func TestExactPublicCompensationRejectsForeignKeys(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	current, _ := item.Public(f.publicName(ref, "cover-230.webp"))
	if err := f.ms.DropUnreferenced(t.Context(), ref, media.Unreferenced{Public: []string{current}}); err != nil {
		t.Fatal(err)
	}
	if !f.exists(current) {
		t.Fatal("compensation deleted a live publication")
	}
	for _, key := range []string{"another/gallery/x/public/cover.webp", item.PublicPrefix() + "../private/secret"} {
		if err := f.ms.DropUnreferenced(t.Context(), ref, media.Unreferenced{Public: []string{key}}); err == nil {
			t.Fatalf("accepted unsafe public key %q", key)
		}
	}
}

func TestRemovedPublicPresetRetiresRecordedGenerations(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	old, _ := item.Public(f.publicName(ref, "cover-230.webp"))
	source, _ := item.Blob(f.fileBlob(ref, "cover.png"))
	cfg := f.reg.Config()
	for i := range cfg.Kinds {
		if cfg.Kinds[i].Name == "gallery" {
			cfg.Kinds[i].Public = nil
		}
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &noDiscoveryStore{Store: f.env.Store}
	m, err := media.NewManifests(store, reg, media.ManifestOptions{Locker: s3test.Locker(t, f.env.Store), Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EditExisting(t.Context(), ref, func(*media.Manifest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.exists(old) || !f.exists(source) || store.lists.Load() != 0 {
		t.Fatal("removed preset lost ownership evidence or deleted the retained source")
	}
}

func TestUnknownAliasRetirementReclaimsLateWrite(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(cfg *media.Config) { cfg.Kinds[0].Public[0].Stable = true })
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	item, _ := f.reg.Item(ref)
	alias, _ := item.Public("cover-230.webp")
	if !f.exists(alias) {
		t.Fatal("stable alias was not produced")
	}
	// A historical manifest has no recorded alias policy. The alias may be
	// temporarily absent while a previously authorized copy is still running.
	if _, err := f.ms.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		for i := range m.Files {
			for j := range m.Files[i].Public {
				m.Files[i].Public[j].Stable = nil
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Store.Delete(t.Context(), alias); err != nil {
		t.Fatal(err)
	}
	cfg := f.reg.Config()
	cfg.Kinds[0].Public = nil
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &noDiscoveryStore{Store: f.env.Store}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: reg, Locker: s3test.Locker(t, f.env.Store), Journal: f.journal, Now: func() time.Time { return time.Now().Add(10 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Manifests().EditExisting(t.Context(), ref, func(*media.Manifest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.object(alias, "late historical alias")
	if n, err := jobs.CleanupDue(t.Context(), 100); err != nil || n == 0 {
		t.Fatalf("late alias cleanup: %d %v", n, err)
	}
	if f.exists(alias) || store.lists.Load() != 0 {
		t.Fatal("historical alias was forgotten or rediscovered through storage listing")
	}
}
