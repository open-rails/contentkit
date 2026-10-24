package openrails_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

// A post's body is the locked bundle's: GET /v1/contents/{id} shows it to
// viewers who open a version (buyers, members, the free post's readers) and to
// editors; everyone else reads the description and the paywall decision.
func TestLockedBodyIntegration(t *testing.T) {
	s := newShop(t)
	studio := s.channel("studio", "Studio", "owner")
	creator, buyer, member, stranger := s.customer(), s.customer(), s.customer(), s.customer()
	post := func(rule access.Rule, body string) string {
		creators := []catalog.Creator{{UserID: creator.ID}}
		return s.post(catalog.ContentInput{Access: &rule, Creators: &creators,
			Text: map[string]catalog.Text{"en": {Title: "Post", Description: "Teaser", Body: body}}})
	}
	free := post(access.Rule{Level: access.Public}, "Free words")
	ppv := post(access.Rule{Level: access.PPV}, "Paid words")
	members := post(access.Rule{Level: access.EntityLevel, Entity: studio}, "Members words")
	s.holds(buyer, ck(ppv))
	s.holds(member, gk(studio))
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
		{"staff read it", s.staff, ppv, "Paid words"},
		{"a stranger is locked out of a members post", stranger, members, ""},
		{"a member reads it", member, members, "Members words"},
	} {
		var got catalog.Content
		raw := s.get(s.catalogAPI, c.viewer, "/contents/"+c.id, &got)
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
	var got catalog.Content
	if s.get(s.catalogAPI, stranger, "/contents/"+ppv, &got); !slices.Equal(got.Decision.Sell, []string{vk(ppv, got.Versions[0].ID), ck(ppv)}) {
		t.Fatalf("the paywall sells %v", got.Decision.Sell)
	}
}
