package openrails_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/testauth"
)

// A post's body is the locked bundle's: GET /v1/contents/{id} shows it to
// viewers who open a version (buyers, members, the free post's readers) and to
// editors; everyone else reads the description and the paywall decision.
func TestLockedBodyIntegration(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	var cat *catalog.Store
	grp, err := groups.New(groups.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds,
		ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
			return cat.ContentChanged(ctx, q, ids)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if cat, err = catalog.New(ctx, catalog.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant, Kinds: w.kinds,
		Auth: testauth.Auth{Authz: catalogStaff{}}, Gate: w.gate, Groups: grp}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	staff := testauth.Actor(access.Actor{ID: "staff"}, catalogStaff{})
	made, err := grp.CreateGroups(ctx, []groups.GroupInput{{Kind: "channel", Slug: "studio", OwnerUserID: "owner",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio"}}}})
	if err != nil {
		t.Fatal(err)
	}
	studio := string(made[0].ID)
	creator, buyer, member, stranger := w.customer(), w.customer(), w.customer(), w.customer()
	post := func(rule access.Rule, body string) string {
		t.Helper()
		creators := []catalog.Creator{{UserID: creator.ID}}
		ids, err := cat.Create(ctx, staff, []catalog.ContentInput{{Kind: "post", Access: &rule, Creators: &creators,
			Text:     map[string]catalog.Text{"en": {Title: "Post", Description: "Teaser", Body: body}},
			Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.Publish(ctx, staff, []catalog.Publish{{ContentID: ids[0]}}); err != nil {
			t.Fatal(err)
		}
		return ids[0]
	}
	free := post(access.Rule{Level: access.Public}, "Free words")
	ppv := post(access.Rule{Level: access.PPV}, "Paid words")
	members := post(access.Rule{Level: access.GroupLevel, Group: studio}, "Members words")
	w.holds(buyer, ck(ppv))
	w.holds(member, gk(studio))

	checker, err := contract.NewChecker(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(checker.Wrap("", http.StripPrefix("/v1", cat.Handler()), func(err error) { t.Error(err) }))
	t.Cleanup(srv.Close)
	get := func(as access.Actor, id string) (catalog.Content, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/contents/"+id, nil)
		testauth.As(req, as)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, resp.StatusCode, raw)
		}
		var c catalog.Content
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c, string(raw)
	}
	anon := access.Actor{Anonymous: true}
	for _, c := range []struct {
		name   string
		viewer access.Actor
		id     string
		body   string
	}{
		{"anyone reads a public post", anon, free, "Free words"},
		{"anonymous is locked out of a PPV post", anon, ppv, ""},
		{"a stranger is locked out of a PPV post", stranger, ppv, ""},
		{"a member is locked out of a PPV post", member, ppv, ""},
		{"the buyer reads it", buyer, ppv, "Paid words"},
		{"its creator reads it", creator, ppv, "Paid words"},
		{"staff read it", staff, ppv, "Paid words"},
		{"a stranger is locked out of a members post", stranger, members, ""},
		{"a member reads it", member, members, "Members words"},
	} {
		got, raw := get(c.viewer, c.id)
		switch {
		case got.Body != c.body:
			t.Errorf("%s: body %q, want %q", c.name, got.Body, c.body)
		case got.Description != "Teaser":
			t.Errorf("%s: description %q", c.name, got.Description)
		case got.Decision.Allowed != (c.body != ""):
			t.Errorf("%s: decision %+v", c.name, got.Decision)
		case c.body == "" && (strings.Contains(raw, " words") || len(got.Decision.Sell) == 0):
			t.Errorf("%s: a locked read carries the body or no paywall: %s", c.name, raw)
		}
	}
	if got, _ := get(stranger, ppv); !slices.Equal(got.Decision.Sell, []string{vk(ppv, got.Versions[0].ID), ck(ppv)}) {
		t.Fatalf("the paywall sells %v", got.Decision.Sell)
	}
}
