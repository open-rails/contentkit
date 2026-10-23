package e2e

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/catalog"
)

// An embedded host's HTTP reader keeps its own continuation. Another read of
// the same page still sees the committed publication; it acknowledges nothing.
func TestCatalogEventReaderKeepsItsOwnCursor(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(nil))
	const events = "/events?types=content.published&limit=1"
	var before catalog.EventPage
	f.want(http.StatusOK, staff, http.MethodGet, events, nil, &before)
	if len(before.Events) != 0 || before.NextCursor == "" {
		t.Fatalf("empty stream %+v", before)
	}
	id, _ := w.published(f, staff, "gallery")
	target := events + "&after=" + url.QueryEscape(before.NextCursor)
	var page, replay, end catalog.EventPage
	f.want(http.StatusOK, staff, http.MethodGet, target, nil, &page)
	if len(page.Events) != 1 || page.Events[0].Type != catalog.EventContentPublished || page.Events[0].ContentID != id || page.HasMore || page.NextCursor == "" || page.RetentionSeconds <= 0 {
		t.Fatalf("publication page %+v", page)
	}
	f.want(http.StatusOK, staff, http.MethodGet, target, nil, &replay)
	if !reflect.DeepEqual(page, replay) {
		t.Fatalf("a read consumed or changed the page: %+v, then %+v", page, replay)
	}
	f.want(http.StatusOK, staff, http.MethodGet, events+"&after="+url.QueryEscape(page.NextCursor), nil, &end)
	if len(end.Events) != 0 || end.HasMore || end.NextCursor == "" {
		t.Fatalf("continued stream %+v", end)
	}
}
