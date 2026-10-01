package image_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// hooked is the real bucket seen through the processor: onPut wraps each
// Put, and the bytes read through Get are counted.
type hooked struct {
	media.Store
	onPut func(key string, put func() error) error
	read  atomic.Int64
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
	p, err := image.New(image.Config{Store: s, Manifests: e.ms, Purge: func(_ context.Context, keys []string) error {
		e.mu.Lock()
		e.purged = append(e.purged, keys...)
		e.mu.Unlock()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// switchable resolves every item visible until hidden is set.
type switchable struct{ hidden *atomic.Bool }

func (s switchable) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		out[r.Key()] = access.Resolution{Visible: !s.hidden.Load(), Accessible: true}
	}
	return out, nil
}

// A hide (Expose) landing while a pass writes public names leaves none of
// them behind, whether the pass then records its work or fails part way
// (no survival until a later pass or the sweep): the pass re-checks Hidden
// under the manifest lock and deletes and purges what it wrote.
func TestHideDuringPass(t *testing.T) {
	hidden := new(atomic.Bool)
	e := newEnv(t, func(c *media.Config) { c.Hooks.Resolver = switchable{hidden} })
	jobs, err := media.NewJobs(media.JobsConfig{Store: e.Store, Registry: e.reg, Locker: s3test.Locker(t, e.Store)})
	if err != nil {
		t.Fatal(err)
	}
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
				// The first name lands, then the item is hidden: Expose records
				// Hidden and deletes public/ while the pass goes on.
				if err := put(); err != nil {
					return err
				}
				hidden.Store(true)
				return jobs.Expose(ctx, ref)
			}
			mu.Lock()
			wrote = append(wrote, key)
			mu.Unlock()
			return put()
		}}
		e.takePurged()
		err := e.processor(t, s).Process(ctx, media.ProcessJob{Ref: ref})
		if failAt > 0 && err == nil {
			t.Fatal("a pass failing part way reported success")
		} else if failAt == 0 && err != nil {
			t.Fatal(err)
		}
		if len(wrote) == 0 {
			t.Fatalf("pass %d wrote nothing after the hide", n)
		}
		for o, err := range e.Store.List(ctx, item.PublicPrefix()) {
			if err != nil {
				t.Fatal(err)
			}
			t.Fatalf("pass %d (fail at %d): %s survived the hide", n, failAt, o.Key)
		}
		purged := e.takePurged()
		for _, k := range wrote {
			if !slices.Contains(purged, k) {
				t.Fatalf("pass %d: %s not purged (%v)", n, k, purged)
			}
		}
		if !e.manifest(t, ref).Hidden {
			t.Fatal("not hidden")
		}
	}
}
