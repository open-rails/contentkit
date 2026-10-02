package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

// The issuance limit counts distinct items per viewer over a sliding hour:
// a repeat is free, a refusal does not count, editors and exempt actors are
// not limited, and the wait it reports is when the next item fits.
func TestIssuance(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0).Truncate(time.Hour)
	now := func() time.Time { return clock }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if newIssuance(Issuance{Disabled: true}, now, log) != nil {
		t.Fatal("a disabled limit")
	}
	var none *issuance
	if err := none.allow(context.Background(), access.Actor{Anonymous: true}, false, "x"); err != nil {
		t.Fatalf("a nil limit refused: %v", err)
	}
	if l := newIssuance(Issuance{}, now, log); l.o.PerHour != 120 || l.o.AnonymousPerHour != 600 {
		t.Fatalf("defaults %+v", l.o)
	}
	l := newIssuance(Issuance{PerHour: 3, AnonymousPerHour: 5, Exempt: func(a access.Actor) bool { return a.Kind == "staff" }}, now, log)
	user, anon := access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true, IP: "203.0.113.7"}
	open := func(a access.Actor, editor bool, item string) (bool, time.Duration) {
		t.Helper()
		err := l.allow(context.Background(), a, editor, item)
		var le *LimitError
		if err != nil && !errors.As(err, &le) {
			t.Fatalf("%+v opening %s: %v", a, item, err)
		}
		if le != nil {
			return false, le.RetryAfter
		}
		return true, 0
	}
	for _, item := range []string{"a", "b", "c", "a", "b"} {
		if ok, _ := open(user, false, item); !ok {
			t.Fatalf("%s refused within the limit", item)
		}
	}
	clock = clock.Add(15 * time.Minute)
	ok, wait := open(user, false, "d")
	// Three items in this hour: the fourth fits a third of the way into the next.
	if ok || wait != 45*time.Minute+20*time.Minute {
		t.Fatalf("a fourth item: allowed %v, wait %v", ok, wait)
	}
	if ok, _ := open(user, false, "a"); !ok {
		t.Fatal("an item opened this hour was refused")
	}
	for i, a := range []access.Actor{{ID: "u2"}, anon, {ID: "s", Kind: "staff"}} {
		if ok, _ := open(a, false, "d"); !ok {
			t.Fatalf("actor %d shares another's count", i)
		}
	}
	if ok, _ := open(user, true, "e"); !ok {
		t.Fatal("an editor of the item was limited")
	}
	for i := range 10 {
		if ok, _ := open(access.Actor{ID: "s", Kind: "staff"}, false, fmt.Sprint("staff", i)); !ok {
			t.Fatal("an exempt actor was limited")
		}
	}
	// Anonymous viewers are counted per IP, at their own limit.
	for i := range 4 {
		if ok, _ := open(anon, false, fmt.Sprint("anon", i)); !ok {
			t.Fatalf("anonymous item %d refused", i)
		}
	}
	if ok, _ := open(anon, false, "anon-over"); ok {
		t.Fatal("a sixth anonymous item allowed")
	}
	if ok, _ := open(access.Actor{Anonymous: true, IP: "203.0.113.8"}, false, "anon-over"); !ok {
		t.Fatal("another address shares the count")
	}
	// The refusal did not count, and the hour slides: at the reported time
	// the item fits, just before it does not.
	clock = clock.Add(wait - time.Second)
	if ok, _ := open(user, false, "d"); ok {
		t.Fatal("allowed before the reported wait")
	}
	clock = clock.Add(time.Second)
	if ok, _ := open(user, false, "d"); !ok {
		t.Fatal("refused at the reported wait")
	}
	// Two hours on, the count is gone.
	clock = clock.Add(2 * time.Hour)
	for _, item := range []string{"x", "y", "z"} {
		if ok, _ := open(user, false, item); !ok {
			t.Fatalf("%s refused after the window passed", item)
		}
	}
	var le *LimitError
	if err := error(&LimitError{RetryAfter: time.Minute}); !errors.Is(err, ErrRateLimited) || !errors.As(err, &le) {
		t.Fatal("LimitError is not ErrRateLimited")
	}
	// An anonymous actor without an IP has no key: every such viewer would
	// share one count, so it is the host's error, not a rate limit. Where
	// the limit does not apply it is not asked for.
	for _, a := range []access.Actor{{Anonymous: true}, {}} {
		err := l.allow(context.Background(), a, false, "x")
		if !errors.Is(err, ErrNoViewerKey) || errors.Is(err, ErrRateLimited) || errors.As(err, &le) {
			t.Fatalf("%+v without an IP: %v", a, err)
		}
		if err := l.allow(context.Background(), a, true, "x"); err != nil {
			t.Fatalf("an editor without an IP: %v", err)
		}
	}
	// An IPv6 client has a /64 of addresses: they are one viewer.
	clock = clock.Add(3 * time.Hour)
	v6 := func(host string) access.Actor { return access.Actor{Anonymous: true, IP: "2001:db8:1:2:" + host} }
	for i := range 5 {
		if ok, _ := open(v6(fmt.Sprintf("%x::%x", i+1, i+7)), false, fmt.Sprint("v6-", i)); !ok {
			t.Fatalf("IPv6 item %d refused", i)
		}
	}
	if ok, _ := open(v6("ffff:ffff:ffff:ffff"), false, "v6-over"); ok {
		t.Fatal("another address of the same /64 got its own count")
	}
	if ok, _ := open(access.Actor{Anonymous: true, IP: "2001:db8:1:3::1"}, false, "v6-over"); !ok {
		t.Fatal("another /64 shares the count")
	}
}

func TestViewerKey(t *testing.T) {
	for _, c := range []struct {
		a    access.Actor
		want string
	}{
		{access.Actor{ID: "u1", IP: "203.0.113.7"}, "a:u1"},
		{access.Actor{Anonymous: true, ID: "u1", IP: "203.0.113.7"}, "ip:203.0.113.7"},
		{access.Actor{IP: "203.0.113.7"}, "ip:203.0.113.7"},
		{access.Actor{Anonymous: true}, ""},
		{access.Actor{}, ""},
		{access.Actor{Anonymous: true, IP: "2001:db8:1:2:3:4:5:6"}, "ip:2001:db8:1:2::/64"},
		{access.Actor{Anonymous: true, IP: "2001:db8:1:2::"}, "ip:2001:db8:1:2::/64"},
		{access.Actor{Anonymous: true, IP: "2001:db8:1:3::1"}, "ip:2001:db8:1:3::/64"},
		{access.Actor{Anonymous: true, IP: "fe80::1%eth0"}, "ip:fe80::/64"},
		{access.Actor{Anonymous: true, IP: "::1"}, "ip:::/64"},
		{access.Actor{Anonymous: true, IP: "::ffff:203.0.113.7"}, "ip:::ffff:203.0.113.7"},
		{access.Actor{Anonymous: true, IP: "not an address"}, "ip:not an address"},
	} {
		if got := viewerKey(c.a); got != c.want {
			t.Errorf("viewerKey(%+v) = %q, want %q", c.a, got, c.want)
		}
	}
}

// The per-process count never holds more than its cap of viewers, however
// many arrive within one window: live keys are evicted to make room, and an
// evicted viewer starts a fresh count.
func TestDistinctCounterIsBounded(t *testing.T) {
	c := &distinctCounter{max: 8, keys: map[string]*distinctWindow{}}
	for i := range 1000 {
		c.add(fmt.Sprint("viewer", i), "item", 7)
		if len(c.keys) > c.max {
			t.Fatalf("%d keys after %d viewers, cap %d", len(c.keys), i+1, c.max)
		}
	}
	// Ended windows go first: the live keys stay.
	c = &distinctCounter{max: 8, keys: map[string]*distinctWindow{}}
	for i := range 4 {
		c.add(fmt.Sprint("old", i), "item", 1)
	}
	for i := range 4 {
		c.add(fmt.Sprint("live", i), "item", 7)
	}
	c.add("new", "item", 7)
	for i := range 4 {
		if cur, _, added := c.add(fmt.Sprint("live", i), "item", 7); added || cur != 1 {
			t.Fatalf("live key %d lost its count while ended windows could go", i)
		}
	}
	if len(c.keys) != 5 {
		t.Fatalf("%d keys, want the 4 live and the new one", len(c.keys))
	}
	if l := newIssuance(Issuance{}, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil))); l.local.max != limiterMaxKeys {
		t.Fatalf("cap %d", l.local.max)
	}
}
