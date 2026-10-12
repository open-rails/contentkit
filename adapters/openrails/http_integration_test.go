package openrails_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/offers"
)

// staff holds root:offers:write.
type staff map[string]bool

func (s staff) Can(_ context.Context, a access.Actor, perm string) (bool, error) {
	return s[a.ID] && perm == string(access.OffersWrite), nil
}

type api struct {
	t   *testing.T
	srv *httptest.Server
}

func (w *world) api(admins ...access.Actor) api {
	w.t.Helper()
	s := staff{}
	for _, a := range admins {
		s[a.ID] = true
	}
	h, err := w.offers.Handler(testauth.Auth{Authz: s})
	if err != nil {
		w.t.Fatal(err)
	}
	checker, err := contract.NewChecker(os.DirFS("../.."))
	if err != nil {
		w.t.Fatal(err)
	}
	srv := httptest.NewServer(checker.Wrap("", h, func(err error) { w.t.Error(err) }))
	w.t.Cleanup(srv.Close)
	return api{t: w.t, srv: srv}
}

func (a api) do(method, path string, as access.Actor, body any, out any) int {
	a.t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, &b)
	testauth.As(req, as)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			a.t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// Creators, group owners and staff put and withdraw offers over HTTP; nobody
// else may.
func TestOfferRoutes(t *testing.T) {
	w := newWorld(t)
	creator, owner, admin, stranger := w.customer(), w.customer(), w.customer(), w.customer()
	a := w.api(admin)
	c := w.content(access.Rule{Level: access.PPV}, "Atlas", creator.ID)
	v := w.version(c, nil, "Uncut")
	channel := w.group("channel", "Night Owls", owner.ID)
	owned := w.content(access.Rule{Level: access.GroupPPV, Group: channel}, "Channel post", "")
	w.exec(`UPDATE `+w.table("content_items")+` SET owner_group_id = $1 WHERE id = $2`, channel, owned)

	var o offers.Offer
	for _, tc := range []struct {
		method, path string
		as           access.Actor
		body         any
		want         int
	}{
		{"PUT", "/contents/" + c + "/offer", access.Actor{Anonymous: true}, map[string]any{"amount": "4.99"}, 401},
		{"PUT", "/contents/" + c + "/offer", stranger, map[string]any{"amount": "4.99"}, 403},
		{"PUT", "/contents/" + c + "/offer", creator, map[string]any{"amount": "4.99", "currency": "EUR"}, 200},
		{"PUT", "/contents/" + c + "/offer", creator, map[string]any{"amount": "0.10"}, 400},
		{"PUT", "/contents/" + c + "/offer", creator, map[string]any{"amount": "4.99", "price": 1}, 400},
		{"PUT", "/contents/" + c + "/versions/" + v + "/offer", creator, map[string]any{"amount": 1.5}, 200},
		{"PUT", "/contents/" + owned + "/versions/" + v + "/offer", admin, map[string]any{"amount": "1.50"}, 404},
		{"PUT", "/contents/" + owned + "/offer", owner, map[string]any{"amount": "2"}, 200},
		{"PUT", "/groups/" + channel + "/offer", stranger, map[string]any{"amount": "9.99", "renew_hours": 720}, 403},
		{"PUT", "/groups/" + channel + "/offer", owner, map[string]any{"amount": "9.99", "renew_hours": 720}, 200},
		{"PUT", "/contents/" + uid(404) + "/offer", admin, map[string]any{"amount": "4.99"}, 404},
		{"DELETE", "/contents/" + c + "/versions/" + v + "/offer", stranger, nil, 403},
		{"DELETE", "/contents/" + c + "/versions/" + v + "/offer", admin, nil, 200},
		{"DELETE", "/contents/" + c + "/versions/" + v + "/offer", admin, nil, 404},
	} {
		if got := a.do(tc.method, tc.path, tc.as, tc.body, &o); got != tc.want {
			t.Errorf("%s %s as %s: %d, want %d", tc.method, tc.path, tc.as.ID, got, tc.want)
		}
	}
	var page offers.OfferPage
	if got := a.do("GET", "/offers?unsynced=true", stranger, nil, &page); got != 403 {
		t.Fatalf("stranger lists offers: %d", got)
	}
	if got := a.do("GET", "/offers?unsynced=true&state=active", admin, nil, &page); got != 200 || len(page.Offers) != 3 {
		t.Fatalf("staff list: %d %+v", got, page)
	}
	i := slices.IndexFunc(page.Offers, func(o offers.Offer) bool { return o.Target.ID == c })
	if i < 0 || page.Offers[i].Terms.Currency != "EUR" || page.Offers[i].Terms.Amount != 4_990_000 {
		t.Fatalf("terms %+v", page.Offers)
	}
}

// A free group's join grants its key through OpenRails, which opens its
// members-only content; leaving takes it back. The library lists what the
// viewer bought, deleted content included, newest first.
func TestJoinAndLibrary(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	owner, viewer := w.customer(), w.customer()
	a := w.api()
	free, paid := w.group("channel", "Free Club", owner.ID), w.group("channel", "Paid Club", owner.ID)
	post := w.content(access.Rule{Level: access.GroupLevel, Group: free}, "For members", "")
	postV := w.version(post, nil, "")

	var m offers.Membership
	if got := a.do("POST", "/groups/"+free+"/join", viewer, nil, &m); got != 409 {
		t.Fatalf("join without an offer: %d", got)
	}
	var o offers.Offer
	if got := a.do("PUT", "/groups/"+free+"/offer", owner, map[string]any{"amount": "0"}, &o); got != 200 {
		t.Fatalf("free offer: %d", got)
	}
	if got := a.do("PUT", "/groups/"+paid+"/offer", owner, map[string]any{"amount": "5"}, &o); got != 200 {
		t.Fatalf("paid offer: %d", got)
	}
	for _, tc := range []struct {
		path string
		as   access.Actor
		want int
	}{
		{"/groups/" + free + "/join", access.Actor{Anonymous: true}, 401},
		{"/groups/" + paid + "/join", viewer, 409},
		{"/groups/" + uid(404) + "/join", viewer, 404},
		{"/groups/" + free + "/join", viewer, 200},
		{"/groups/" + free + "/join", viewer, 200},
	} {
		if got := a.do("POST", tc.path, tc.as, nil, &m); got != tc.want {
			t.Errorf("join %s as %s: %d, want %d", tc.path, tc.as.ID, got, tc.want)
		}
	}
	if !m.Member || m.Key.String() != gk(free) || !w.holdsNow(viewer, gk(free)) {
		t.Fatalf("joined %+v holds %v", m, w.holdsNow(viewer, gk(free)))
	}
	if p, prices := w.product(gk(free)); p.Archived || len(live(prices)) != 0 {
		t.Fatalf("a free group sells nothing: %+v %+v", p, prices)
	}
	ds, err := w.gate.Decide(ctx, viewer, w.items(postV))
	if err != nil || !ds[0].Allowed {
		t.Fatalf("member: %+v %v", ds, err)
	}
	if got := a.do("DELETE", "/groups/"+free+"/join", viewer, nil, &m); got != 200 || m.Member || w.holdsNow(viewer, gk(free)) {
		t.Fatalf("leave: %d %+v", got, m)
	}

	// The library: a content key, a version key and deleted content.
	first := w.content(access.Rule{Level: access.PPV}, "First", "")
	second := w.content(access.Rule{Level: access.PPV}, "Second", "")
	secondV := w.version(second, nil, "")
	gone := w.content(access.Rule{Level: access.PPV}, "Gone", "")
	w.exec(`INSERT INTO `+w.table("content_text")+` (tenant_id, content_id, language, title) VALUES ($1, $2, 'ja', '二番目')`, tenant, second)
	w.exec(`UPDATE `+w.table("content_items")+` SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone)
	w.holds(viewer, ck(first), vk(second, secondV), ck(gone))
	var lib offers.LibraryPage
	if got := a.do("GET", "/me/library?limit=2&language=ja", viewer, nil, &lib); got != 200 {
		t.Fatalf("library: %d", got)
	}
	if len(lib.Items) != 2 || lib.Items[0].ContentID != gone || !lib.Items[0].Deleted || !lib.Items[0].Whole ||
		lib.Items[1].ContentID != second || lib.Items[1].Whole || !slices.Equal(lib.Items[1].VersionIDs, []string{secondV}) ||
		lib.Items[1].Title != "二番目" || !lib.Complete || lib.NextCursor == "" {
		t.Fatalf("library page 1: %+v", lib)
	}
	cursor := lib.NextCursor
	lib = offers.LibraryPage{}
	if got := a.do("GET", "/me/library?limit=2&cursor="+cursor, viewer, nil, &lib); got != 200 || len(lib.Items) != 1 ||
		lib.Items[0].ContentID != first || lib.Items[0].Title != "First" || lib.NextCursor != "" {
		t.Fatalf("library page 2: %d %+v", got, lib)
	}
	if got := a.do("GET", "/me/library", access.Actor{Anonymous: true}, nil, nil); got != 401 {
		t.Fatalf("anonymous library: %d", got)
	}
}
