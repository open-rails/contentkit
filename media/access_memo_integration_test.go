package media_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/access/accesstest"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

type gateFacts func(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error)

func (f gateFacts) Facts(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error) {
	return f(ctx, refs, actor)
}

// The read handler runs each request in access.WithMemo: with a Gate
// resolver whose Facts reads the viewer's Filter, one media read is one
// billing read.
func TestReadHandlerMemoizesGateReads(t *testing.T) {
	held := &accesstest.Entitlements{}
	count := &accesstest.CountingEntitlements{Next: held}
	var gate *access.Gate
	facts := gateFacts(func(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Fact, error) {
		fl, _ := gate.Filter(ctx, actor)
		out := map[contentref.ContentKey]access.Fact{}
		for _, r := range refs {
			out[r.Key()] = access.Fact{Visible: true, Bypass: fl.Tier("staff_pass"), Rule: access.Rule{Level: access.PPV}}
		}
		return out, nil
	})
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		var err error
		gate, err = access.NewGate(access.GateConfig{Entitlements: count, Tenant: c.Namespace,
			Tiers: []string{"staff_pass"}, OwnedKinds: []string{"gallery"}})
		if err != nil {
			t.Fatal(err)
		}
		c.Hooks.Resolver = gate.Resolver(facts)
	})
	owned, other := f.ref("gallery", 1), f.ref("gallery", 2)
	viewer := access.Actor{ID: "0192aaaa-0000-7000-8000-000000000001", Kind: "user"}
	held.Grant(viewer.ID, access.Own(owned).String())
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{viewer}}))
	defer srv.Close()
	for _, tc := range []struct {
		ref  contentref.ContentRef
		full bool
	}{{owned, true}, {other, false}} {
		count.Reset()
		resp, err := http.Get(srv.URL + "/gallery/" + tc.ref.ContentID)
		if err != nil {
			t.Fatal(err)
		}
		var res media.ReadResult
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || (res.Access == "full") != tc.full {
			t.Fatalf("%s: status %d access %q err %v", tc.ref, resp.StatusCode, res.Access, err)
		}
		if count.Calls() != 1 {
			t.Fatalf("%s: %d billing reads in one media read, want 1", tc.ref, count.Calls())
		}
	}
}
