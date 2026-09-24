package accessworker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/redis/go-redis/v9"
)

// limitReplicas returns n read API handlers ("replicas") over one reader with
// the same limit, and a request helper.
func limitReplicas(t *testing.T, n int, limit media.ViewerLimit) ([]http.Handler, func(h http.Handler, actor string) *httptest.ResponseRecorder) {
	env := s3test.Open(t)
	kinds, err := media.NewRegistry(media.Kind{Name: "post", Specs: map[string]media.Spec{"large": {}}})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	reader, err := media.NewReader(media.ReaderOptions{Manifests: ms, Kinds: kinds,
		Resolver: verdicts{cid(1): {Visible: true, Accessible: true}},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.example", SigningKey: k2}})
	if err != nil {
		t.Fatal(err)
	}
	hs := make([]http.Handler, n)
	for i := range hs {
		hs[i] = reader.Handler(media.HandlerOptions{Tenant: env.Tenant, Identity: actorHeader{}, Limit: limit})
	}
	return hs, func(h http.Handler, actor string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/post/"+cid(1), nil)
		req = req.WithContext(context.WithValue(req.Context(), actorHeader{}, access.Actor{ID: actor}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
}

func randID(t *testing.T) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// redisURLs lists CONTENTKIT_TEST_REDIS_URLS (Redis and Garnet in CI); unset skips.
func redisURLs(t *testing.T) []string {
	var out []string
	for _, u := range strings.Split(os.Getenv("CONTENTKIT_TEST_REDIS_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		t.Skip("CONTENTKIT_TEST_REDIS_URLS not set")
	}
	return out
}

// TestSharedViewerRateLimit: replicas sharing one Redis (or Garnet) enforce
// one combined limit per viewer, with expiring, prefixed keys.
func TestSharedViewerRateLimit(t *testing.T) {
	for _, u := range redisURLs(t) {
		t.Run(u, func(t *testing.T) {
			opt, err := redis.ParseURL(u)
			if err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(opt)
			t.Cleanup(func() { rdb.Close() })
			ctx := context.Background()
			if err := rdb.Ping(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			prefix := "cktest:" + randID(t) + ":"
			before := media.RedisErrors.Value()
			// Window = 5/1 s: the sequence below runs well inside it.
			hs, get := limitReplicas(t, 2, media.ViewerLimit{PerSecond: 1, Burst: 5, Redis: rdb, KeyPrefix: prefix})
			for i := range 5 {
				if rec := get(hs[i%2], "scraper"); rec.Code != http.StatusOK {
					t.Fatalf("request %d on replica %d: %d %s", i, i%2, rec.Code, rec.Body)
				}
			}
			for i := range 2 {
				rec := get(hs[i], "scraper")
				if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rate_limited") {
					t.Fatalf("replica %d over the combined limit: %d %v %s", i, rec.Code, rec.Header(), rec.Body)
				}
			}
			if rec := get(hs[1], "someone-else"); rec.Code != http.StatusOK {
				t.Fatalf("another viewer: %d", rec.Code)
			}
			if got := media.RedisErrors.Value(); got != before {
				t.Fatalf("redis errors %d -> %d", before, got)
			}
			keys, err := rdb.Keys(ctx, prefix+"*").Result()
			if err != nil || len(keys) == 0 {
				t.Fatalf("keys under %q: %v %v", prefix, keys, err)
			}
			for _, k := range keys {
				if ttl, err := rdb.PTTL(ctx, k).Result(); err != nil || ttl <= 0 || ttl > 12*time.Second {
					t.Fatalf("%s ttl %v %v", k, ttl, err)
				}
			}
		})
	}
}

// TestSharedViewerRateLimitSlides: once the window moves on, the viewer is
// allowed again at the sustained rate, not a fresh burst.
func TestSharedViewerRateLimitSlides(t *testing.T) {
	for _, u := range redisURLs(t) {
		t.Run(u, func(t *testing.T) {
			opt, err := redis.ParseURL(u)
			if err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(opt)
			t.Cleanup(func() { rdb.Close() })
			// Window = 4/10 s = 400ms.
			hs, get := limitReplicas(t, 2, media.ViewerLimit{PerSecond: 10, Burst: 4, Redis: rdb, KeyPrefix: "cktest:" + randID(t) + ":"})
			allowed := 0
			for i := range 8 {
				if get(hs[i%2], "scraper").Code == http.StatusOK {
					allowed++
				}
			}
			if allowed > 5 { // 4, or 5 when the sequence straddles a window edge
				t.Fatalf("allowed %d of 8 at a burst of 4", allowed)
			}
			time.Sleep(900 * time.Millisecond) // two windows: fully refilled
			for i := range 4 {
				if rec := get(hs[i%2], "scraper"); rec.Code != http.StatusOK {
					t.Fatalf("after the window, request %d: %d", i, rec.Code)
				}
			}
		})
	}
}

// TestViewerRateLimitRedisDown: an unreachable Redis fails open to each
// process's own limit and counts the error.
func TestViewerRateLimitRedisDown(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { rdb.Close() })
	before := media.RedisErrors.Value()
	hs, get := limitReplicas(t, 2, media.ViewerLimit{PerSecond: 0.001, Burst: 3, Redis: rdb})
	for _, h := range hs {
		for i := range 3 {
			if rec := get(h, "viewer"); rec.Code != http.StatusOK {
				t.Fatalf("request %d with Redis down: %d %s", i, rec.Code, rec.Body)
			}
		}
		if rec := get(h, "viewer"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("per-process limit with Redis down: %d", rec.Code)
		}
	}
	if media.RedisErrors.Value() == before {
		t.Fatal("redis errors not counted")
	}
}

// TestViewerRateLimitPerProcess: without Redis each replica keeps its own
// limit, as before.
func TestViewerRateLimitPerProcess(t *testing.T) {
	hs, get := limitReplicas(t, 2, media.ViewerLimit{PerSecond: 0.001, Burst: 2})
	for _, h := range hs {
		for i := range 2 {
			if rec := get(h, "viewer"); rec.Code != http.StatusOK {
				t.Fatalf("request %d: %d", i, rec.Code)
			}
		}
		if rec := get(h, "viewer"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("over the per-process limit: %d", rec.Code)
		}
	}
}
