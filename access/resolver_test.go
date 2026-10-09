package access_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

type factsFunc func(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error)

func (f factsFunc) Facts(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error) {
	return f(ctx, refs, actor)
}

// Accessible = Visible && !Withheld && (Editor || Bypass || allowed), for
// every combination, in one Facts call and at most one billing read.
func TestResolverTruthTable(t *testing.T) {
	w := newWorld(t, nil)
	facts := map[contentref.ContentKey]access.Fact{}
	var refs []contentref.ContentRef
	want := map[contentref.ContentKey]bool{}
	n := 0
	for _, visible := range []bool{false, true} {
		for _, withheld := range []bool{false, true} {
			for _, editor := range []bool{false, true} {
				for _, bypass := range []bool{false, true} {
					for _, allowed := range []bool{false, true} {
						n++
						ref := post(n)
						if allowed {
							w.grant(access.Own(ref))
						}
						refs = append(refs, ref)
						facts[ref.Key()] = access.Fact{Visible: visible, Withheld: withheld, Editor: editor, Bypass: bypass,
							Owner: "owner", Rule: access.Rule{Level: access.PPV}}
						want[ref.Key()] = visible && !withheld && (editor || bypass || allowed)
					}
				}
			}
		}
	}
	omitted := post(999)
	refs = append(refs, omitted)
	factCalls := 0
	r := w.gate.Resolver(factsFunc(func(_ context.Context, got []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		factCalls++
		if len(got) != len(refs) {
			t.Fatalf("Facts got %d refs", len(got))
		}
		return facts, nil
	}))
	res, err := r.Resolve(t.Context(), refs, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if factCalls != 1 {
		t.Fatalf("Facts calls %d", factCalls)
	}
	w.calls(t, 1)
	for key, acc := range want {
		got := res[key]
		if got.Accessible != acc || got.Visible != facts[key].Visible || got.Editor != facts[key].Editor || got.Owner != "owner" {
			t.Errorf("%v: %+v, want accessible=%v", key, got, acc)
		}
	}
	if _, ok := res[omitted.Key()]; ok {
		t.Fatal("an omitted fact must stay omitted (denied)")
	}

	// A failed billing read denies the keyed items without failing the batch.
	w.held.Fail(errors.New("down"))
	res, err = r.Resolve(t.Context(), refs, viewer)
	if err != nil {
		t.Fatal(err)
	}
	for key, f := range facts {
		if acc := res[key].Accessible; acc != (f.Visible && !f.Withheld && (f.Editor || f.Bypass)) {
			t.Errorf("%v during outage: accessible=%v", key, acc)
		}
	}

	// A canonical Ref decides access on its work; facts errors fail the batch.
	w.held.Fail(nil)
	alias := contentref.New(tenant, "post", uid(5000))
	canonical := post(5001)
	w.grant(access.Own(canonical))
	r = w.gate.Resolver(factsFunc(func(context.Context, []contentref.ContentRef, access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		return map[contentref.ContentKey]access.Fact{alias.Key(): {Ref: canonical, Visible: true, Rule: access.Rule{Level: access.PPV}}}, nil
	}))
	res, err = r.Resolve(t.Context(), []contentref.ContentRef{alias}, viewer)
	if err != nil || !res[alias.Key()].Accessible || !res[alias.Key()].Ref.Equal(canonical) {
		t.Fatalf("canonical: %+v %v", res, err)
	}
	r = w.gate.Resolver(factsFunc(func(context.Context, []contentref.ContentRef, access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		return nil, fmt.Errorf("db down")
	}))
	if _, err := r.Resolve(t.Context(), refs, viewer); err == nil {
		t.Fatal("a Facts error must fail the batch")
	}
}

// A versioned media item is decided on its work's key.
func TestResolverDecidesOnTheWork(t *testing.T) {
	w := newWorld(t, nil)
	version := contentref.NewVersion(tenant, "post", uid(1), "en-1")
	w.grant(access.Own(version.Content()))
	r := w.gate.Resolver(factsFunc(func(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		return map[contentref.ContentKey]access.Fact{refs[0].Key(): {Visible: true, Rule: access.Rule{Level: access.PPV}}}, nil
	}))
	res, err := access.ResolveOne(t.Context(), r, version, viewer)
	if err != nil || !res.Accessible {
		t.Fatalf("%+v %v", res, err)
	}
}
