package discovery

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/internal/testauth"
)

func (f *fixture) guestCall(method, host, user, path, header string, body any, out any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(method, "http://"+host+path, bytes.NewReader(raw))
	if header != "" {
		r.Header.Set(readerprefs.Header, header)
	}
	if user != "" {
		r = testauth.As(r, access.Actor{ID: user})
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			f.t.Fatal(err)
		}
	}
	return w
}

func TestGuestContentFiltersApplyWithoutAccountRows(t *testing.T) {
	f := seeded(t)
	choice := ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: true}, FilterSetting{EntityID: f.ids["yaoi"], Visible: false})}
	raw, _ := json.Marshal(choice.Filters)
	header := `{"filters":` + string(raw) + `}`
	var settings ContentFilters
	if w := f.guestCall(http.MethodGet, "main.test", "", "/content-filters", header, nil, &settings); w.Code != http.StatusOK || !slices.Contains(w.Header().Values("Vary"), readerprefs.Header) {
		t.Fatalf("guest settings: %d %s", w.Code, w.Body)
	}
	if len(settings.Filters) != 2 || !settings.Filters[0].Visible || !settings.Filters[0].Overridden || settings.Filters[1].Visible || !settings.Filters[1].Overridden || settings.IncludeDeleted {
		t.Fatalf("guest choices: %+v", settings)
	}
	var page ContentPage
	if w := f.guestCall(http.MethodGet, "main.test", "", q("/contents", "kind", "gallery"), header, nil, &page); w.Code != http.StatusOK {
		t.Fatalf("guest list: %d %s", w.Code, w.Body)
	}
	got := []string{}
	for _, c := range page.Contents {
		got = append(got, c.ID)
	}
	if !reflect.DeepEqual(got, []string{f.cid(2), f.cid(1)}) {
		t.Fatalf("guest list: %v", got)
	}
	if page.Contents[0].Access.Allowed {
		t.Fatal("a content filter granted premium access")
	}
	// A hard entrypoint restriction still applies even when local preferences
	// request content outside it.
	if w := f.guestCall(http.MethodGet, "gay.test", "", q("/contents", "kind", "gallery"), header, nil, &page); w.Code != http.StatusOK || len(page.Contents) != 1 || page.Contents[0].ID != f.cid(3) {
		t.Fatalf("hard domain scope: %d %+v", w.Code, page)
	}
	// An independent guest request has defaults, not the previous guest's data.
	if w := f.guestCall(http.MethodGet, "main.test", "", "/content-filters", "", nil, &settings); w.Code != http.StatusOK || settings.Filters[0].Visible || settings.Filters[0].Overridden {
		t.Fatalf("guest state leaked: %d %+v", w.Code, settings)
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+f.table("content_filter_prefs")).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("guest rows=%d err=%v", rows, err)
	}
	if w := f.guestCall(http.MethodPatch, "main.test", "", "/me/content-filters", header, choice, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("guest wrote preferences: %d", w.Code)
	}
}

func TestGuestContentFiltersValidateAndIgnoreHeaderForAccounts(t *testing.T) {
	f := seeded(t)
	for _, header := range []string{`no`, `null`, `{"filters":[{"entity_id":"` + f.ids["yaoi"] + `","visible":true},{"entity_id":"` + f.ids["yaoi"] + `","visible":false}]}`, `{"filters":[],"include_deleted":true}`} {
		for _, path := range []string{"/content-filters", "/contents?kind=gallery", "/search?q=alpha"} {
			w := f.guestCall(http.MethodGet, "main.test", "", path, header, nil, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid header %q at %s: %d %s", header, path, w.Code, w.Body)
			}
		}
	}
	update := ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: true}, FilterSetting{EntityID: f.ids["yaoi"], Visible: false})}
	if err := f.svc.SetContentFilters(f.ctx, access.Actor{ID: "reader"}, update); err != nil {
		t.Fatal(err)
	}
	var settings ContentFilters
	w := f.guestCall(http.MethodGet, "main.test", "reader", "/content-filters", "malformed guest state", nil, &settings)
	if w.Code != http.StatusOK || !settings.Filters[0].Visible || settings.Filters[1].Visible {
		t.Fatalf("account authority: %d %+v", w.Code, settings)
	}
	var page ContentPage
	w = f.guestCall(http.MethodGet, "main.test", "reader", "/contents?kind=gallery", "malformed guest state", nil, &page)
	if w.Code != http.StatusOK || len(page.Contents) != 2 || page.Contents[0].ID != f.cid(2) {
		t.Fatalf("account list: %d %+v", w.Code, page)
	}
}

func TestInitializeContentFiltersPreservesExistingSettings(t *testing.T) {
	f := seeded(t)
	first := ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: true})}
	second := ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: false})}
	var settings ContentFilters
	for _, body := range []ContentFiltersUpdate{first, second} {
		w := f.guestCall(http.MethodPost, "main.test", "new-account", "/me/content-filters/initialize", "", ContentFiltersInitialization{AccountID: "new-account", Filters: body.Filters}, &settings)
		if w.Code != http.StatusOK || !settings.Filters[0].Visible {
			t.Fatalf("initialize replay overwrote first choice: %d %+v", w.Code, settings)
		}
	}
	// An explicitly saved all-default account is still initialized. A delayed
	// registration callback must not replace that user's choices.
	if err := f.svc.SetContentFilters(f.ctx, access.Actor{ID: "existing-account"}, ContentFiltersUpdate{}); err != nil {
		t.Fatal(err)
	}
	w := f.guestCall(http.MethodPost, "main.test", "existing-account", "/me/content-filters/initialize", "", ContentFiltersInitialization{AccountID: "existing-account", Filters: first.Filters}, &settings)
	if w.Code != http.StatusOK || settings.Filters[0].Visible || settings.Filters[0].Overridden {
		t.Fatalf("saved defaults overwritten: %d %+v", w.Code, settings)
	}
	if w := f.guestCall(http.MethodPost, "main.test", "", "/me/content-filters/initialize", "", ContentFiltersInitialization{AccountID: "new-account", Filters: first.Filters}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("guest initialization: %d", w.Code)
	}
	if w := f.guestCall(http.MethodPost, "main.test", "other-account", "/me/content-filters/initialize", "", ContentFiltersInitialization{AccountID: "new-account", Filters: first.Filters}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("cross-account initialization: %d %s", w.Code, w.Body)
	}
	if w := f.guestCall(http.MethodPost, "main.test", "other-account", "/me/content-filters/initialize", "", ContentFiltersInitialization{}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unbound initialization: %d %s", w.Code, w.Body)
	}
	var foreignRows int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+f.table("content_filter_prefs")+" WHERE user_id='other-account'").Scan(&foreignRows); err != nil || foreignRows != 0 {
		t.Fatalf("cross-account write: %d %v", foreignRows, err)
	}
	// Concurrent registration callbacks write one complete selection; neither
	// can replace the winner or merge a partial set into it.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, body := range []ContentFiltersUpdate{first, second} {
		wg.Go(func() {
			errs <- f.svc.InitializeContentFilters(f.ctx, access.Actor{ID: "race-account"}, ContentFiltersInitialization{AccountID: "race-account", Filters: body.Filters})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+f.table("content_filter_prefs")+" WHERE user_id='race-account'").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("initialization rows=%d err=%v", rows, err)
	}
	before, err := f.svc.ContentFilters(f.ctx, access.Actor{ID: "race-account"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.InitializeContentFilters(f.ctx, access.Actor{ID: "race-account"}, ContentFiltersInitialization{AccountID: "race-account"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.ContentFilters(f.ctx, access.Actor{ID: "race-account"}, "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("initialized settings changed: %+v %+v %v", before, after, err)
	}
}
