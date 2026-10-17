package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
)

// Admin quick navigation matches current names of drafts and internal
// entities within each section's own authority: contents need write or a
// credit, scoped before the limit; entities need entity management. Public
// typeahead offers neither.
func TestManagementSearchSuggestions(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	creatorUser, creator := w.person(iam.Role{})
	_, stranger := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	word := "quartz" + uuid.NewString()[:8]
	draft := func(title string, creators ...string) string {
		in := catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: title}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}
		if len(creators) > 0 {
			cs := make([]catalog.Creator, len(creators))
			for i, c := range creators {
				cs[i] = catalog.Creator{UserID: c}
			}
			in.Creators = &cs
		}
		var c catalog.Content
		f.want(http.StatusCreated, staff, "POST", "/contents", in, &c)
		return c.ID
	}
	// Staff drafts match as prefixes and outrank the credited draft's substring match.
	drafts := []string{draft(word + " Alpha"), draft(word + " Beta")}
	credited := draft("Credited "+word, creatorUser.ID)
	var created []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/entities", []groups.GroupInput{{Kind: "tag", Slug: word, Internal: true,
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: word + " Marker"}}}}, &created)
	internal := created[0].ID

	suggest := func(status int, bearer string, q url.Values) discovery.Typeahead {
		t.Helper()
		var out discovery.Typeahead
		var into any
		if status == http.StatusOK {
			into = &out
		}
		f.want(status, bearer, "GET", "/manage/search-suggestions?"+q.Encode(), nil, into)
		return out
	}
	ids := func(cs []discovery.Card) []string {
		out := []string{}
		for _, c := range cs {
			out = append(out, c.ID)
		}
		slices.Sort(out)
		return out
	}
	both := url.Values{"q": {word}, "include_contents": {"true"}, "include_entities": {"true"}}
	suggest(http.StatusUnauthorized, "", both)
	suggest(http.StatusBadRequest, staff, url.Values{"q": {word}})

	got := suggest(http.StatusOK, staff, both)
	want := append(slices.Clone(drafts), credited)
	slices.Sort(want)
	if !slices.Equal(ids(got.Contents), want) || len(got.Groups) != 1 || got.Groups[0].ID != internal || !got.Groups[0].Internal {
		t.Fatalf("staff quick navigation: contents %v, entities %+v", ids(got.Contents), got.Groups)
	}

	limited := url.Values{"q": {word}, "include_contents": {"true"}, "include_entities": {"true"}, "limit": {"1"}}
	if got = suggest(http.StatusOK, creator, limited); !slices.Equal(ids(got.Contents), []string{credited}) || len(got.Groups) != 0 {
		t.Fatalf("creator quick navigation at limit 1: contents %v, entities %+v", ids(got.Contents), got.Groups)
	}

	if got = suggest(http.StatusOK, stranger, url.Values{"q": {word}, "include_contents": {"true"}}); len(got.Contents) != 0 {
		t.Fatalf("an uncredited person found drafts: %v", ids(got.Contents))
	}
	suggest(http.StatusForbidden, stranger, url.Values{"q": {word}, "include_entities": {"true"}})

	var public discovery.Typeahead
	f.want(http.StatusOK, "", "GET", "/typeahead?q="+word, nil, &public)
	for _, c := range public.Contents {
		if c.ID == credited || slices.Contains(drafts, c.ID) {
			t.Fatalf("public typeahead offered draft %s", c.ID)
		}
	}
	for _, g := range public.Groups {
		if g.ID == internal {
			t.Fatalf("public typeahead offered internal entity %s", g.ID)
		}
	}
}
