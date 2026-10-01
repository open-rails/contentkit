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
	if ok, _ := none.allow(context.Background(), access.Actor{ID: "u"}, false, "x"); !ok {
		t.Fatal("a nil limit refused")
	}
	if l := newIssuance(Issuance{}, now, log); l.o.PerHour != 120 || l.o.AnonymousPerHour != 600 {
		t.Fatalf("defaults %+v", l.o)
	}
	l := newIssuance(Issuance{PerHour: 3, AnonymousPerHour: 5, Exempt: func(a access.Actor) bool { return a.Kind == "staff" }}, now, log)
	user, anon := access.Actor{ID: "u1", Kind: "user"}, access.Actor{Anonymous: true, IP: "203.0.113.7"}
	open := func(a access.Actor, editor bool, item string) (bool, time.Duration) {
		return l.allow(context.Background(), a, editor, item)
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
}
