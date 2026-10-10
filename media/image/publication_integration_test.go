package image_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// retiring reports every public delete before it is sent.
type retiring struct {
	*hooked
	onDelete func(key string)
}

func (s *retiring) Delete(ctx context.Context, key string) error {
	s.onDelete(key)
	return s.hooked.Delete(ctx, key)
}

// A republish keeps the current cover published while its successor is
// written: readers and the projection keep the old generation, cleanup keeps
// its files, and they are retired only after the projection names the new one.
func TestRepublishMovesProjectionBeforeRetiring(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: ref})
	item, _ := e.reg.Item(ref)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	first, ok := e.file(t, ref, "cover.png").Current("cover")
	if !ok {
		t.Fatal("no published cover")
	}
	projected := func() []string {
		images, err := e.ms.PresetImages(context.WithoutCancel(ctx), "cover", ref)
		if err != nil {
			t.Error(err)
			return nil
		}
		var urls []string
		for _, r := range images[0].Renditions {
			urls = append(urls, r.URL)
		}
		return urls
	}
	urlsOf := func(p media.Publication) []string {
		var out []string
		for _, name := range p.NamesOnDisk() {
			out = append(out, e.reg.PublicURL(ref, name))
		}
		return out
	}
	if got := projected(); !slices.Equal(got, urlsOf(first)) {
		t.Fatalf("projection %v, published %v", got, urlsOf(first))
	}

	started, resume := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var release sync.Once
	defer release.Do(func() { close(resume) })
	var mu sync.Mutex
	var retired, early []string
	s := &retiring{hooked: &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
		if strings.HasPrefix(key, item.PublicPrefix()) && paused.CompareAndSwap(false, true) {
			close(started)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return put()
	}}, onDelete: func(key string) {
		name := strings.TrimPrefix(key, item.PublicPrefix())
		if !slices.Contains(first.NamesOnDisk(), name) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		retired = append(retired, name)
		if slices.Contains(projected(), e.reg.PublicURL(ref, name)) {
			early = append(early, name)
		}
	}}
	ms := s3test.Manifests(t, s, e.reg, media.ManifestOptions{Journal: e.journal})
	worker, err := image.New(image.Config{Store: s, Manifests: ms})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Process(ctx, media.ProcessJob{Ref: ref, Force: true, Preset: "cover"}) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("worker exited before writing its successor: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f := e.file(t, ref, "cover.png")
	current, _ := f.Current("cover")
	next, _ := f.Publication("cover")
	if current.Generation != first.Generation || next.Ready() || next.Generation == first.Generation {
		t.Fatalf("while writing: current %+v, successor %+v", current, next)
	}
	if keys, err := ms.SyncPublic(ctx, ref); err != nil || len(keys) != 0 {
		t.Fatalf("cleanup during the successor's write: %v %v", keys, err)
	}
	if got := projected(); !slices.Equal(got, urlsOf(first)) {
		t.Fatalf("projection while writing: %v", got)
	}
	for _, name := range first.NamesOnDisk() {
		if _, _, ok := e.public(t, ref, name); !ok {
			t.Fatalf("current %s went missing while its successor was written", name)
		}
	}
	release.Do(func() { close(resume) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second, _ := e.file(t, ref, "cover.png").Current("cover")
	if second.Generation != next.Generation {
		t.Fatalf("published %+v, reserved %+v", second, next)
	}
	if got := projected(); !slices.Equal(got, urlsOf(second)) {
		t.Fatalf("projection %v, published %v", got, urlsOf(second))
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(retired)
	want := first.NamesOnDisk()
	slices.Sort(want)
	if !slices.Equal(retired, want) || len(early) != 0 {
		t.Fatalf("retired %v (want %v); while still projected: %v", retired, want, early)
	}
	for _, name := range want {
		key, _ := item.Public(name)
		if _, err := e.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("superseded %s: %v", key, err)
		}
	}
}
