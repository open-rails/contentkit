package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
)

// Explicit history IDs may retain an unlisted work through the real AuthKit
// middleware and checked HTTP contract, while ordinary browsing still omits it.
func TestKnownContentCardsKeepUnlistedHistoryUnderRealAuth(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, reader := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	listed, _ := w.published(f, staff, "gallery")
	unlisted, _ := w.published(f, staff, "gallery")
	listing := catalog.ListingUnlisted
	f.want(http.StatusOK, staff, "PATCH", "/contents/"+unlisted, catalog.ContentPatch{Listing: &listing}, nil)

	query := url.Values{"ids": {unlisted + "," + listed}, "kind": {"gallery"}}
	ids := func(page discovery.ContentPage) []string {
		out := make([]string, len(page.Contents))
		for i, card := range page.Contents {
			out[i] = card.ID
		}
		return out
	}
	for _, actor := range []struct{ name, token string }{{"guest", ""}, {"signed-in reader", reader}} {
		var browse discovery.ContentPage
		f.want(http.StatusOK, actor.token, "GET", "/contents?"+query.Encode(), nil, &browse)
		if got := ids(browse); !slices.Equal(got, []string{listed}) {
			t.Fatalf("%s browse returned %v, want only listed %s", actor.name, got, listed)
		}

		var known discovery.ContentPage
		f.want(http.StatusOK, actor.token, "GET", "/content-cards?"+query.Encode(), nil, &known)
		if got := ids(known); !slices.Equal(got, []string{unlisted, listed}) {
			t.Fatalf("%s known cards returned %v, want caller order [%s %s]", actor.name, got, unlisted, listed)
		}
		for _, card := range known.Contents {
			if !card.Access.Allowed || card.Title == "" {
				t.Fatalf("%s public known card lost its access or title: %+v", actor.name, card)
			}
		}
	}

	for _, invalid := range []string{"", "not-an-id", strings.Repeat(listed+",", 100) + listed} {
		f.want(http.StatusBadRequest, "", "GET", "/content-cards?"+url.Values{"ids": {invalid}}.Encode(), nil, nil)
	}
	f.want(http.StatusUnauthorized, reader+"forged", "GET", "/content-cards?"+query.Encode(), nil, nil)
}
