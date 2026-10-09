package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/access/accesstest"
	"github.com/open-rails/contentkit/contentref"
)

type gateFacts func(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error)

func (f gateFacts) Facts(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error) {
	return f(ctx, refs, actor)
}

// The handler runs each request in access.WithMemo: a host's Facts that
// reads the viewer's Filter (a staff pass granted as an entitlement) and the
// Decide behind the Gate resolver share one billing read per request.
func TestHandlerMemoizesGateReads(t *testing.T) {
	held := &accesstest.Entitlements{}
	count := &accesstest.CountingEntitlements{Next: held}
	gate, err := access.NewGate(access.GateConfig{Entitlements: count, Tenant: testTenant,
		Tiers: []string{"premium", "staff_pass"}, OwnedKinds: []string{"widget"}})
	if err != nil {
		t.Fatal(err)
	}
	facts := gateFacts(func(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		f, _ := gate.Filter(ctx, actor)
		out := map[contentref.ContentKey]access.Fact{}
		for _, r := range refs {
			out[r.Key()] = access.Fact{Visible: true, Bypass: f.Tier("staff_pass"),
				Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}}
		}
		return out, nil
	})
	resolver := gate.Resolver(facts)
	rt, _ := newTestRuntime(t, Options{Resolver: resolver, ContentKinds: []string{"widget"}})
	h := rt.Handler()
	like := func(actor access.Actor) int {
		req := httptest.NewRequest("POST", "/widget/"+cid(1)+"/like", nil)
		req = req.WithContext(withActor(req.Context(), actor))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	premium := access.Actor{ID: "0192aaaa-0000-7000-8000-000000000001", Kind: "user"}
	free := access.Actor{ID: "0192aaaa-0000-7000-8000-000000000002", Kind: "user"}
	held.Grant(premium.ID, access.Tier("premium").String())
	for _, tc := range []struct {
		actor access.Actor
		want  int
	}{{premium, http.StatusOK}, {free, http.StatusForbidden}} {
		count.Reset()
		if got := like(tc.actor); got != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.actor.ID, got, tc.want)
		}
		if count.Calls() != 1 {
			t.Fatalf("%s: %d billing reads in one request, want 1", tc.actor.ID, count.Calls())
		}
	}
	// The same resolve outside a memo is two reads.
	count.Reset()
	if _, err := access.ResolveOne(t.Context(), resolver, ref("widget", cid(1)), free); err != nil {
		t.Fatal(err)
	}
	if count.Calls() != 2 {
		t.Fatalf("unmemoized resolve: %d reads, want 2", count.Calls())
	}
}
