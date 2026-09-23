package tiered

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/access"
)

// entitlements is an in-memory Checker: subject -> held keys.
type entitlements struct {
	held  map[string][]string
	calls [][]string
	err   error
}

func (e *entitlements) Check(_ context.Context, subject string, keys []string) (map[string]bool, error) {
	e.calls = append(e.calls, keys)
	if e.err != nil {
		return nil, e.err
	}
	out := map[string]bool{}
	for _, k := range e.held[subject] {
		out[k] = true
	}
	return out, nil
}

const (
	membership = "channel:c1:membership"
	purchase   = "post:p1"
	premium    = "site:premium"
)

func policy(l Level) Policy {
	return Policy{Level: l, Membership: membership, Purchase: purchase, Premium: premium}
}

func TestDecideLevels(t *testing.T) {
	ctx := context.Background()
	c := &entitlements{held: map[string][]string{
		"member": {membership}, "buyer": {purchase}, "premium": {premium},
	}}
	want := map[string]map[Level]bool{
		"nobody":  {Public: true},
		"member":  {Public: true, Members: true},
		"buyer":   {Public: true, Members: true, PPV: true, MembersPPV: true, Premium: true},
		"premium": {Public: true, Premium: true},
	}
	for subject, levels := range want {
		for _, l := range []Level{Public, Members, PPV, MembersPPV, Premium} {
			got, err := Decide(ctx, c, access.Actor{ID: subject}, policy(l))
			if err != nil || got != levels[l] {
				t.Errorf("%s/%s = %v, %v; want %v", subject, l, got, err, levels[l])
			}
		}
	}
}

func TestMembersPPVIgnoresMembership(t *testing.T) {
	c := &entitlements{held: map[string][]string{"member": {membership}}}
	ok, err := Decide(context.Background(), c, access.Actor{ID: "member"}, policy(MembersPPV))
	if err != nil || ok {
		t.Fatalf("member without purchase = %v, %v; want denied", ok, err)
	}
	if !reflect.DeepEqual(c.calls, [][]string{{purchase}}) {
		t.Fatalf("checked %v; want only the purchase key", c.calls)
	}
}

func TestAnonymousNeverChecks(t *testing.T) {
	c := &entitlements{err: errors.New("must not be called")}
	for _, a := range []access.Actor{{Anonymous: true, IP: "1.2.3.4"}, {}} {
		got, err := DecideAll(context.Background(), c, a, []Policy{policy(Public), policy(Members), policy(PPV)})
		if err != nil || !reflect.DeepEqual(got, []bool{true, false, false}) {
			t.Fatalf("anonymous = %v, %v", got, err)
		}
	}
	if len(c.calls) != 0 {
		t.Fatalf("checker called for anonymous: %v", c.calls)
	}
}

func TestCheckerErrorDenies(t *testing.T) {
	boom := errors.New("openrails down")
	c := &entitlements{err: boom}
	ok, err := Decide(context.Background(), c, access.Actor{ID: "u"}, policy(Members))
	if ok || !errors.Is(err, boom) {
		t.Fatalf("= %v, %v; want denied with checker error", ok, err)
	}
	got, err := DecideAll(context.Background(), c, access.Actor{ID: "u"}, []Policy{policy(Public), policy(PPV)})
	if got != nil || !errors.Is(err, boom) {
		t.Fatalf("DecideAll = %v, %v; want nil with checker error", got, err)
	}
}

func TestInvalidPolicyDenies(t *testing.T) {
	c := &entitlements{}
	for _, p := range []Policy{{Level: "vip", Purchase: purchase}, {Level: Members, Purchase: purchase}, {Level: PPV}, {Level: Premium}} {
		if ok, err := Decide(context.Background(), c, access.Actor{ID: "buyer"}, p); ok || !errors.Is(err, ErrPolicy) {
			t.Errorf("%+v = %v, %v; want ErrPolicy", p, ok, err)
		}
	}
	if len(c.calls) != 0 {
		t.Fatalf("checker called for invalid policy: %v", c.calls)
	}
}

func TestDecideAllBatchesDistinctKeys(t *testing.T) {
	c := &entitlements{held: map[string][]string{"u": {"post:p2", membership}}}
	ps := []Policy{
		policy(Members),
		{Level: PPV, Purchase: "post:p2"},
		{Level: MembersPPV, Membership: membership, Purchase: "post:p3"},
		policy(Public),
	}
	got, err := DecideAll(context.Background(), c, access.Actor{ID: "u"}, ps)
	if err != nil || !reflect.DeepEqual(got, []bool{true, true, false, true}) {
		t.Fatalf("= %v, %v", got, err)
	}
	if want := [][]string{{membership, purchase, "post:p2", "post:p3"}}; !reflect.DeepEqual(c.calls, want) {
		t.Fatalf("calls %v; want %v", c.calls, want)
	}
}
