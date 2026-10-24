package openrails_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/search"
)

// A channel's feed (owner_group) lists only the channel's posts, under every
// other filter, the listing's visibility and the access mode. Owning is not
// membership: the channel's key opens its members-only posts, not its PPV or
// members-only PPV ones, and carries no owner into group filters.
func TestOwnerEntityFeedIntegration(t *testing.T) {
	s := newShop(t)
	ctx := t.Context()
	member, stranger := s.customer(), s.customer()
	studio, other := s.channel("studio", "Studio", "owner"), s.channel("other", "Other", "owner")
	none := []catalog.Creator{}
	post := func(owner string, rule access.Rule, title string) string {
		return s.post(catalog.ContentInput{Access: &rule, OwnerEntityID: owner, Creators: &none, Text: map[string]catalog.Text{"en": {Title: title}}})
	}
	free := post(studio, access.Rule{Level: access.Public}, "Night free")
	ppv := post(studio, access.Rule{Level: access.PPV}, "Night PPV")
	members := post(studio, access.Rule{Level: access.EntityLevel, Entity: studio}, "Night members")
	membersPPV := post(studio, access.Rule{Level: access.EntityPPV, Entity: studio}, "Night members PPV")
	elsewhere := post(other, access.Rule{Level: access.Public}, "Night elsewhere")
	unowned := post("", access.Rule{Level: access.Public}, "Night unowned")
	s.holds(member, gk(studio))

	type card struct {
		id      string
		allowed bool
	}
	list := func(as access.Actor, path string) []card {
		t.Helper()
		var page discovery.ContentPage
		s.get(s.discoveryAPI, as, path, &page)
		out := []card{}
		for _, c := range page.Contents {
			out = append(out, card{c.ID, c.Access.Allowed})
		}
		slices.SortFunc(out, func(a, b card) int { return strings.Compare(a.id, b.id) })
		return out
	}
	want := func(cs ...card) []card {
		slices.SortFunc(cs, func(a, b card) int { return strings.Compare(a.id, b.id) })
		return cs
	}
	anon := access.Actor{Anonymous: true}
	feed := "/contents?kind=post&owner_group=" + studio
	for _, c := range []struct {
		name   string
		viewer access.Actor
		path   string
		want   []card
	}{
		{"anyone lists the channel's posts, locked", anon, feed,
			want(card{free, true}, card{ppv, false}, card{members, false}, card{membersPPV, false})},
		{"hide lists what anyone opens", anon, feed + "&mode=hide", want(card{free, true})},
		{"the channel's key opens its members post only", member, feed,
			want(card{free, true}, card{ppv, false}, card{members, true}, card{membersPPV, false})},
		{"a member's hidden feed", member, feed + "&mode=hide", want(card{free, true}, card{members, true})},
		{"a stranger's hidden feed", stranger, feed + "&mode=hide", want(card{free, true})},
		{"two channels", anon, "/contents?kind=post&mode=hide&owner_group=" + studio + "," + other, want(card{free, true}, card{elsewhere, true})},
		{"owning is no membership", anon, "/contents?kind=post&groups=" + studio, want()},
		{"every post", anon, "/contents?kind=post&mode=hide", want(card{free, true}, card{elsewhere, true}, card{unowned, true})},
	} {
		if got := list(c.viewer, c.path); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// Search and the release date index take the filter too.
	var docs []search.KeywordDocument
	for _, id := range []string{free, ppv, elsewhere, unowned} {
		docs = append(docs, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: s.cat.Ref("post", id), Language: "en"}, Title: "Night"})
	}
	if err := search.UpsertKeywordDocuments(ctx, s.pool, s.schema, docs); err != nil {
		t.Fatal(err)
	}
	if got := list(anon, "/search?q=night&language=en&owner_group="+studio); !slices.Equal(got, want(card{free, true}, card{ppv, false})) {
		t.Errorf("search: %v", got)
	}
	var dates discovery.ReleaseDates
	s.get(s.discoveryAPI, anon, "/release-dates?kind=post&owner_group="+studio, &dates)
	if len(dates.Dates) != 1 || dates.Dates[0].Count != 4 || dates.Dates[0].Free != 1 {
		t.Errorf("release dates %+v", dates)
	}
}
