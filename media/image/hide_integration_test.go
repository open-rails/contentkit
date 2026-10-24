package image_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
)

// hooked is the real bucket seen through the processor: onPut wraps each
// Put, and the bytes read through Get are counted.
type hooked struct {
	media.Store
	onPut         func(key string, put func() error) error
	onGet         func(key string)
	read          atomic.Int64
	omitChecksums bool
}

func (s *hooked) Head(ctx context.Context, key string) (media.Object, error) {
	obj, err := s.Store.Head(ctx, key)
	if s.omitChecksums {
		obj.ChecksumSHA256 = nil
	}
	return obj, err
}

func (s *hooked) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	var obj media.Object
	put := func() (err error) {
		obj, err = s.Store.Put(ctx, key, body, size, o)
		return err
	}
	if s.onPut == nil {
		return obj, put()
	}
	return obj, s.onPut(key, put)
}

func (s *hooked) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	rc, obj, err := s.Store.Get(ctx, key, o)
	if err != nil {
		return rc, obj, err
	}
	if s.onGet != nil {
		s.onGet(key)
	}
	return counted{rc, &s.read}, obj, nil
}

type counted struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// processor is a Processor over s instead of the env's store.
func (e *env) processor(t *testing.T, s media.Store) *image.Processor {
	t.Helper()
	p, err := image.New(image.Config{Store: s, Manifests: e.ms})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// switchable resolves every item visible until hidden is set.
type switchable struct{ hidden *atomic.Bool }

func (s switchable) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	out := map[contentref.ContentKey]resolve.Resolution{}
	for _, r := range refs {
		out[r.Key()] = resolve.Resolution{Visible: !s.hidden.Load(), Accessible: true}
	}
	return out, nil
}

// Hiding during public publication removes every written name, including
// when publication fails partway through.
func TestHideDuringPass(t *testing.T) {
	hidden := new(atomic.Bool)
	e := newEnv(t, func(c *media.Config) {
		c.Hooks.Resolver = switchable{hidden}
	})
	ctx := context.Background()
	for n, failAt := range []int64{0, 3} {
		hidden.Store(false)
		ref := e.ref(t, "gallery", n+1)
		item, _ := e.reg.Item(ref)
		e.put(t, ref, "cover.png", "image/png", quadrants(t))
		var mu sync.Mutex
		var puts atomic.Int64
		var wrote []string
		s := &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
			if !strings.HasPrefix(key, item.PublicPrefix()) {
				return put()
			}
			switch puts.Add(1) {
			case failAt:
				return errors.New("bucket unavailable")
			case 1:
				// Expose waits for the in-flight publication lock.
				if err := put(); err != nil {
					return err
				}
				hidden.Store(true)
				return nil
			}
			mu.Lock()
			wrote = append(wrote, key)
			mu.Unlock()
			return put()
		}}
		err := e.processor(t, s).Process(ctx, media.ProcessJob{Ref: ref})
		if !hidden.Load() {
			t.Fatalf("publication did not reach the hide hook: %v", err)
		}
		if failAt > 0 && err == nil {
			t.Fatal("a pass failing part way reported success")
		} else if failAt == 0 && err != nil {
			t.Fatal(err)
		}
		if len(wrote) == 0 {
			t.Fatalf("pass %d did not exercise the remaining public writes", n)
		}
		if err := e.processor(t, e.Store).Process(ctx, media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, err := range e.Store.List(ctx, item.PublicPrefix()) {
			if err != nil {
				t.Fatal(err)
			}
			count++
		}
		if count == 0 {
			t.Fatal("visibility change removed public presets")
		}
		if e.manifest(t, ref).Hidden {
			t.Fatal("visibility changed manifest")
		}
	}
}
