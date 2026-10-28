package critical_test

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// The limiter keeps the same log in process and in Redis; CI runs the
// in-process store, and CONTENTKIT_CRITICAL_REDIS runs the same cases against
// a shared one.
func TestRateLimiter(t *testing.T) {
	var rdb redis.UniversalClient
	if addr := os.Getenv("CONTENTKIT_CRITICAL_REDIS"); addr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = rdb.Close() })
	}
	now := time.UnixMilli(1_800_000_000_000)
	clock := func() time.Time { return now }
	limiter := func(t *testing.T, windows ...ratelimit.Window) *ratelimit.Limiter {
		l, err := ratelimit.New(ratelimit.Options{Windows: windows, Redis: rdb, Prefix: "ck-test:" + uuid.NewString() + ":", Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	expect := func(t *testing.T, ok bool, wait time.Duration, wantOK bool, wantWait time.Duration) {
		t.Helper()
		if ok != wantOK || wait != wantWait {
			t.Fatalf("got ok=%v wait=%s, want ok=%v wait=%s", ok, wait, wantOK, wantWait)
		}
	}

	t.Run("CK-J13-S09_limit_admits_burst_per_window_and_refusals_do_not_count", func(t *testing.T) {
		ctx := t.Context()
		l := limiter(t, ratelimit.Window{Limit: 3, Per: 10 * time.Second})
		start := now
		for range 3 {
			ok, wait := l.Allow(ctx, "viewer")
			expect(t, ok, wait, true, 0)
		}
		ok, wait := l.Allow(ctx, "viewer")
		expect(t, ok, wait, false, 10*time.Second)
		now = start.Add(4 * time.Second)
		ok, wait = l.Allow(ctx, "viewer")
		expect(t, ok, wait, false, 6*time.Second)
		ok, wait = l.Allow(ctx, "other")
		expect(t, ok, wait, true, 0)
		// The refusals were not logged: once the first three slide out, three
		// more fit.
		now = start.Add(10*time.Second + time.Millisecond)
		for range 3 {
			ok, wait = l.Allow(ctx, "viewer")
			expect(t, ok, wait, true, 0)
		}
		ok, _ = l.Allow(ctx, "viewer")
		if ok {
			t.Fatal("a fourth request fit the window")
		}
	})

	t.Run("CK-J13-S09_every_window_must_admit", func(t *testing.T) {
		ctx := t.Context()
		l := limiter(t, ratelimit.Window{Limit: 2, Per: time.Second}, ratelimit.Window{Limit: 3, Per: time.Minute})
		start := now
		for range 2 {
			ok, wait := l.Allow(ctx, "viewer")
			expect(t, ok, wait, true, 0)
		}
		ok, wait := l.Allow(ctx, "viewer")
		expect(t, ok, wait, false, time.Second)
		now = start.Add(time.Second + time.Millisecond)
		ok, wait = l.Allow(ctx, "viewer")
		expect(t, ok, wait, true, 0)
		ok, wait = l.Allow(ctx, "viewer")
		expect(t, ok, wait, false, time.Minute-time.Second-time.Millisecond)
	})

	t.Run("CK-J13-S09_member_limit_counts_distinct_items_from_their_first_opening", func(t *testing.T) {
		ctx := t.Context()
		l := limiter(t, ratelimit.Window{Limit: 2, Per: time.Hour})
		start := now
		ok, wait := l.AllowMember(ctx, "viewer", "a")
		expect(t, ok, wait, true, 0)
		now = start.Add(time.Minute)
		ok, wait = l.AllowMember(ctx, "viewer", "b")
		expect(t, ok, wait, true, 0)
		ok, wait = l.AllowMember(ctx, "viewer", "a")
		expect(t, ok, wait, true, 0)
		ok, wait = l.AllowMember(ctx, "viewer", "c")
		expect(t, ok, wait, false, time.Hour-time.Minute)
		// The refused item was not logged, so reopening a and b stays free.
		ok, wait = l.AllowMember(ctx, "viewer", "b")
		expect(t, ok, wait, true, 0)
		now = start.Add(time.Hour + time.Millisecond)
		ok, wait = l.AllowMember(ctx, "viewer", "c")
		expect(t, ok, wait, true, 0)
		// a slid out an hour after it was first opened, so it counts again.
		ok, wait = l.AllowMember(ctx, "viewer", "a")
		expect(t, ok, wait, false, time.Minute-time.Millisecond)
	})
}
