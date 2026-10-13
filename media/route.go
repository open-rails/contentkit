package media

import (
	"context"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Route* serve a namespace bound to its owner's journal (PGJournal.Bind)
// from the owner's bucket, worker queue and encode progress, and every other
// namespace from the app's own.

// routes is own plus each bound namespace's owner.
type routes[T comparable] struct {
	own   T
	bound map[string]T
}

func newRoutes[T comparable](what string, own T, bound map[string]T) (routes[T], error) {
	var none T
	if own == none {
		return routes[T]{}, fmt.Errorf("media: routing needs its own %s", what)
	}
	r := routes[T]{own: own, bound: make(map[string]T, len(bound))}
	for ns, v := range bound {
		if !layout.ValidSegment(ns) || v == none {
			return routes[T]{}, fmt.Errorf("media: invalid %s route for namespace %q", what, ns)
		}
		r.bound[ns] = v
	}
	return r, nil
}

func (r routes[T]) at(ns string) T {
	if v, ok := r.bound[ns]; ok {
		return v
	}
	return r.own
}

// RouteStore serves each bound namespace's keys from its owner's bucket.
func RouteStore(own Store, bound map[string]Store) (Store, error) {
	r, err := newRoutes("store", own, bound)
	if err != nil {
		return nil, err
	}
	return &routeStore{r}, nil
}

type routeStore struct{ routes[Store] }

func (r *routeStore) at(key string) Store {
	ns, _, _ := strings.Cut(key, "/")
	return r.routes.at(ns)
}

func (r *routeStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) (Object, error) {
	return r.at(key).Put(ctx, key, body, size, opts)
}

func (r *routeStore) Get(ctx context.Context, key string, opts GetOptions) (io.ReadCloser, Object, error) {
	return r.at(key).Get(ctx, key, opts)
}

func (r *routeStore) Head(ctx context.Context, key string) (Object, error) {
	return r.at(key).Head(ctx, key)
}

func (r *routeStore) Delete(ctx context.Context, key string) error {
	return r.at(key).Delete(ctx, key)
}

func (r *routeStore) List(ctx context.Context, prefix string) iter.Seq2[Object, error] {
	return r.at(prefix).List(ctx, prefix)
}

func (r *routeStore) Copy(ctx context.Context, src, dst string, o CopyOptions) (Object, error) {
	s := r.at(dst)
	if r.at(src) != s {
		return Object{}, fmt.Errorf("media: copy across buckets: %s -> %s", src, dst)
	}
	return s.Copy(ctx, src, dst, o)
}

func (r *routeStore) PresignPut(ctx context.Context, key string, p PresignPut) (PresignedRequest, error) {
	return r.at(key).PresignPut(ctx, key, p)
}

func (r *routeStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (PresignedRequest, error) {
	return r.at(key).PresignGet(ctx, key, ttl)
}

func (r *routeStore) CreateMultipart(ctx context.Context, key, contentType string) (string, error) {
	return r.at(key).CreateMultipart(ctx, key, contentType)
}

func (r *routeStore) PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, sha256 []byte, ttl time.Duration) (PresignedRequest, error) {
	return r.at(key).PresignPart(ctx, key, uploadID, number, size, sha256, ttl)
}

func (r *routeStore) PutPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64, sha256 []byte) (Part, error) {
	return r.at(key).PutPart(ctx, key, uploadID, number, body, size, sha256)
}

func (r *routeStore) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	return r.at(key).ListParts(ctx, key, uploadID)
}

func (r *routeStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error) {
	return r.at(key).CompleteMultipart(ctx, key, uploadID, parts)
}

func (r *routeStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	return r.at(key).AbortMultipart(ctx, key, uploadID)
}

// Capabilities are the weakest common to every bucket.
func (r *routeStore) Capabilities() Capabilities {
	c := r.own.Capabilities()
	for _, s := range r.bound {
		b := s.Capabilities()
		c.ConditionalPut = c.ConditionalPut && b.ConditionalPut
		c.ChecksumSHA256 = c.ChecksumSHA256 && b.ChecksumSHA256
	}
	return c
}

func (r *routeStore) Check(ctx context.Context, prefix string) error {
	if err := r.own.Check(ctx, prefix); err != nil {
		return err
	}
	for ns, s := range r.bound {
		if err := s.Check(ctx, ns+"/_probe/"); err != nil {
			return err
		}
	}
	return nil
}

// RouteQueue enqueues each bound namespace's processing in its owner's
// worker queue.
func RouteQueue(own ProcessQueue, bound map[string]ProcessQueue) (ProcessQueue, error) {
	r, err := newRoutes("queue", own, bound)
	if err != nil {
		return nil, err
	}
	return routeQueue{r}, nil
}

type routeQueue struct{ routes[ProcessQueue] }

func (r routeQueue) Enqueue(ctx context.Context, job ProcessJob) error {
	return r.at(job.Ref.TenantID).Enqueue(ctx, job)
}

// RouteProgress reads each bound namespace's encode progress from its
// owner's worker.
func RouteProgress(own ProgressSource, bound map[string]ProgressSource) (ProgressSource, error) {
	r, err := newRoutes("progress source", own, bound)
	if err != nil {
		return nil, err
	}
	return routeProgress{r}, nil
}

type routeProgress struct{ routes[ProgressSource] }

func (r routeProgress) EncodeProgress(ctx context.Context, ref contentref.ContentRef) (EncodeStatus, error) {
	return r.at(ref.TenantID).EncodeProgress(ctx, ref)
}
