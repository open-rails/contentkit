package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// limitFixture is one replica's runtime over a seeded schema: two galleries,
// a published post, a poll and a comment.
type limitFixture struct {
	rt      *Runtime
	h       http.Handler
	post    string
	poll    pollView
	comment string
}

// newLimitFixture builds a runtime with limits; with seed it also seeds
// the content (once per schema), otherwise it reuses from's schema and ids.
func newLimitFixture(t *testing.T, limits Limits, from *limitFixture) limitFixture {
	t.Helper()
	opts := Options{Limits: limits, ContentKinds: []string{"gallery", "post"}, Perms: Perms{PostWrite: "post", PollWrite: "poll"}}
	if from != nil {
		opts.Schema, opts.Resolver = from.rt.schema, from.rt.resolver
		rt, _ := newTestRuntime(t, opts)
		f := *from
		f.rt, f.h = rt, rt.Handler()
		return f
	}
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	res.set("gallery", cid(2), true, true)
	opts.Resolver = res
	rt, _ := newTestRuntime(t, opts)
	f := limitFixture{rt: rt, h: rt.Handler()}
	ctx := context.Background()
	admin := access.Actor{ID: "admin", Kind: "user"}
	rec := doJSON(t, f.h, admin, "POST", "/posts", postWriteReq{Title: ptr("Hello"), Body: ptr("world"), IsDraft: ptr(false)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create post: %d %s", rec.Code, rec.Body.String())
	}
	f.post = decodePost(t, rec).ID
	res.set("post", f.post, true, true)
	var err error
	if f.poll, err = rt.polls.create(ctx, admin, twoOptionPoll("en")); err != nil {
		t.Fatal(err)
	}
	f.comment = mustComment(t, rt, admin, "gallery", cid(1), createInput{Body: "first"}).ID
	return f
}

type step struct {
	method, path string
	body         any
}

// assertLimited checks a 429: the Retry-After header (whole seconds within
// [lo, hi]) and the typed body naming action.
func assertLimited(t *testing.T, rec *httptest.ResponseRecorder, action Action, lo, hi int) {
	t.Helper()
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	got := decodeErr(t, rec.Body.String())
	if rec.Code != http.StatusTooManyRequests || err != nil || ra < lo || ra > hi ||
		got.Code != CodeRateLimited || got.Action != action || got.RetryAfter != ra {
		t.Fatalf("want 429 %s retry-after in [%d,%d]; got %d Retry-After %q %s", action, lo, hi, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// Every interaction has its own per-actor budget; undoing spends like doing;
// a refusal is a typed 429 with Retry-After; other actors (anonymous ones by
// IP) keep their own budgets.
func TestRateLimitsPerActionAndChurn(t *testing.T) {
	two := []Rate{{2, time.Hour}}
	f := newLimitFixture(t, Limits{Comment: two, CommentReaction: two, PostReaction: two, Reaction: two, Favorite: two, PollVote: two}, nil)
	bot := access.Actor{ID: "bot", Kind: "user"}
	g1, g2 := "/gallery/"+cid(1), "/gallery/"+cid(2)
	opt := map[string]string{"option_id": f.poll.Options[0].ID}
	for _, c := range []struct {
		action Action
		steps  [3]step
	}{
		{ActionComment, [3]step{{"POST", g1 + "/comments", createInput{Body: "a"}}, {"POST", g1 + "/comments", createInput{Body: "b"}}, {"POST", g2 + "/comments", createInput{Body: "c"}}}},
		{ActionCommentReaction, [3]step{{"POST", "/comments/" + f.comment + "/like", nil}, {"POST", "/comments/" + f.comment + "/neutral", nil}, {"POST", "/comments/" + f.comment + "/dislike", nil}}},
		// The post routes and the generic route on kind post share one budget.
		{ActionPostReaction, [3]step{{"POST", "/posts/" + f.post + "/like", nil}, {"POST", "/post/" + f.post + "/neutral", nil}, {"POST", "/posts/" + f.post + "/dislike", nil}}},
		{ActionReaction, [3]step{{"POST", g1 + "/like", nil}, {"DELETE", g1 + "/reaction", nil}, {"POST", g2 + "/dislike", nil}}},
		{ActionFavorite, [3]step{{"POST", g1 + "/favorite", nil}, {"DELETE", g1 + "/favorite", nil}, {"POST", g1 + "/favorite", nil}}},
		// A free-text answer spends the vote budget, even one refused for
		// the poll's kind.
		{ActionPollVote, [3]step{{"POST", "/polls/" + f.poll.ID + "/vote", opt}, {"POST", "/polls/" + f.poll.ID + "/answer", map[string]string{"text": "x"}}, {"POST", "/polls/" + f.poll.ID + "/vote", opt}}},
	} {
		t.Run(string(c.action), func(t *testing.T) {
			for i, s := range c.steps[:2] {
				if rec := doJSON(t, f.h, bot, s.method, s.path, s.body); rec.Code == http.StatusTooManyRequests || rec.Code >= 500 {
					t.Fatalf("step %d %s %s: %d %s", i, s.method, s.path, rec.Code, rec.Body.String())
				}
			}
			s := c.steps[2]
			assertLimited(t, doJSON(t, f.h, bot, s.method, s.path, s.body), c.action, 3590, 3600)
		})
	}
	if rec := doJSON(t, f.h, access.Actor{ID: "human", Kind: "user"}, "POST", g1+"/comments", createInput{Body: "hi"}); rec.Code != http.StatusCreated {
		t.Fatalf("another user's comment: %d %s", rec.Code, rec.Body.String())
	}
	a, b := access.Actor{Anonymous: true, IP: "10.0.0.1"}, access.Actor{Anonymous: true, IP: "10.0.0.2"}
	for i := range 2 {
		if rec := doJSON(t, f.h, a, "POST", g2+"/like", nil); rec.Code != http.StatusOK {
			t.Fatalf("anonymous like %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	assertLimited(t, doJSON(t, f.h, a, "POST", g2+"/like", nil), ActionReaction, 3590, 3600)
	if rec := doJSON(t, f.h, b, "POST", g2+"/like", nil); rec.Code != http.StatusOK {
		t.Fatalf("anonymous like from another IP: %d %s", rec.Code, rec.Body.String())
	}
}

// Comments pass a burst rate and a sustained one: the burst refusal frees
// within its window, the sustained one after the hour.
func TestRateLimitCommentBurstAndSustained(t *testing.T) {
	f := newLimitFixture(t, Limits{Comment: []Rate{{2, 3 * time.Second}, {3, time.Hour}}}, nil)
	bot := access.Actor{ID: "bot", Kind: "user"}
	post := func() *httptest.ResponseRecorder {
		return doJSON(t, f.h, bot, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "spam"})
	}
	for i := range 2 {
		if rec := post(); rec.Code != http.StatusCreated {
			t.Fatalf("comment %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	assertLimited(t, post(), ActionComment, 1, 3)
	deadline := time.Now().Add(5 * time.Second)
	for rec := post(); rec.Code != http.StatusCreated; rec = post() {
		if rec.Code != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("after the burst window: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(100 * time.Millisecond) // refusals are not counted
	}
	assertLimited(t, post(), ActionComment, 3590, 3600)
}

// The zero Limits take the defaults; a malformed rate refuses construction.
func TestRateLimitDefaults(t *testing.T) {
	f := newLimitFixture(t, Limits{KeyPrefix: "defaults:"}, nil)
	bot := access.Actor{ID: "bot", Kind: "user"}
	for i := range 5 {
		if rec := doJSON(t, f.h, bot, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "c" + strconv.Itoa(i)}); rec.Code != http.StatusCreated {
			t.Fatalf("comment %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	assertLimited(t, doJSON(t, f.h, bot, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "six"}), ActionComment, 290, 300)
	for i := range 20 {
		method := []string{"POST", "DELETE"}[i%2]
		if rec := doJSON(t, f.h, bot, method, "/gallery/"+cid(1)+"/favorite", nil); rec.Code != http.StatusOK {
			t.Fatalf("favorite toggle %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	assertLimited(t, doJSON(t, f.h, bot, "POST", "/gallery/"+cid(1)+"/favorite", nil), ActionFavorite, 50, 60)

	_, err := New(context.Background(), Options{Pool: f.rt.store.pool, Schema: f.rt.schema, Tenant: testTenant, Identity: &fakeIdentity{},
		Authz: allowAll{}, Resolver: &fakeResolver{}, Limits: Limits{Favorite: []Rate{{0, time.Minute}}}})
	if err == nil || !strings.Contains(err.Error(), "favorite") {
		t.Fatalf("a zero-count rate: %v", err)
	}
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

// Replicas sharing one Redis (or Garnet) enforce one budget per actor, in
// expiring, prefixed keys that keep only admitted events. With Redis down a
// replica counts on its own and says so.
func TestRateLimitSharedAcrossReplicas(t *testing.T) {
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
			prefix := "cktest:" + contentref.NewID() + ":"
			limits := func(c redis.UniversalClient) Limits {
				return Limits{Comment: []Rate{{3, time.Hour}}, Favorite: []Rate{{2, time.Hour}}, Redis: c, KeyPrefix: prefix}
			}
			a := newLimitFixture(t, limits(rdb), nil)
			b := newLimitFixture(t, limits(rdb), &a)
			bot := access.Actor{ID: "bot", Kind: "user"}
			comment := func(f limitFixture, who access.Actor) *httptest.ResponseRecorder {
				return doJSON(t, f.h, who, "POST", "/gallery/"+cid(1)+"/comments", createInput{Body: "spam"})
			}
			before := RateLimitRedisErrors.Value()
			for i, f := range []limitFixture{a, b, a} {
				if rec := comment(f, bot); rec.Code != http.StatusCreated {
					t.Fatalf("comment %d: %d %s", i, rec.Code, rec.Body.String())
				}
			}
			for _, f := range []limitFixture{b, a} {
				assertLimited(t, comment(f, bot), ActionComment, 3590, 3600)
			}
			if rec := comment(b, access.Actor{ID: "human", Kind: "user"}); rec.Code != http.StatusCreated {
				t.Fatalf("another user: %d %s", rec.Code, rec.Body.String())
			}
			for i, s := range []step{{"POST", "/gallery/" + cid(1) + "/favorite", nil}, {"DELETE", "/gallery/" + cid(1) + "/favorite", nil}} {
				if rec := doJSON(t, []limitFixture{a, b}[i].h, bot, s.method, s.path, nil); rec.Code != http.StatusOK {
					t.Fatalf("favorite step %d: %d %s", i, rec.Code, rec.Body.String())
				}
			}
			assertLimited(t, doJSON(t, a.h, bot, "POST", "/gallery/"+cid(1)+"/favorite", nil), ActionFavorite, 3590, 3600)
			if got := RateLimitRedisErrors.Value(); got != before {
				t.Fatalf("redis errors %d -> %d", before, got)
			}
			key := prefix + testTenant + ":comment:{u:bot}:3600000"
			if n, err := rdb.ZCard(ctx, key).Result(); err != nil || n != 3 {
				t.Fatalf("events kept under %s: %d %v", key, n, err)
			}
			if ttl, err := rdb.PTTL(ctx, key).Result(); err != nil || ttl <= 0 || ttl > time.Hour+time.Second {
				t.Fatalf("%s ttl %v %v", key, ttl, err)
			}

			dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
			t.Cleanup(func() { dead.Close() })
			alone := newLimitFixture(t, limits(dead), &a)
			for i := range 3 {
				if rec := comment(alone, bot); rec.Code != http.StatusCreated {
					t.Fatalf("with Redis down, comment %d: %d %s", i, rec.Code, rec.Body.String())
				}
			}
			assertLimited(t, comment(alone, bot), ActionComment, 3590, 3600)
			if RateLimitRedisErrors.Value() == before {
				t.Fatal("the Redis failure was not counted")
			}
		})
	}
}
