package discovery

import (
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/signal"
)

func strp(s string) *string { return &s }

// seeded is a small site: galleries in English and Japanese, a premium
// one behind a default-hidden content filter, a restricted one in the yaoi
// tag, and ones no listing shows.
func seeded(t *testing.T) *fixture { return seededOn(t, nil, "") }

func seededOn(t *testing.T, ch signal.Conn, chDB string) *fixture {
	f := newFixture(t, ch, chDB)
	f.group("tag", "romance", "big-breasts", "yaoi")
	f.group("artist", "alice")
	f.group("series", "saga")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Alpha", "ja": "アルファ"}, age: 72 * time.Hour, favorites: 5, attrs: map[string]any{"format": "manga"},
			popularity: `{"score":{"7d":1.5,"30d":3},"views":40}`,
			versions:   []ver{{langs: []string{"en"}, pages: 20}, {langs: []string{"ja"}, traits: []string{"colored"}}}, groups: []string{"romance", "alice", "saga"}},
		item{n: 2, titles: map[string]string{"en": "Bravo"}, age: 48 * time.Hour, rule: `{"level":"tier","tiers":["premium"],"preview":{"pages":3}}`,
			favorites: 9, popularity: `{"score":{"7d":9}}`, versions: []ver{{langs: []string{"en"}}}, groups: []string{"big-breasts"}},
		item{n: 3, titles: map[string]string{"ja": "チャーリー"}, age: 24 * time.Hour, restrictions: []string{"restricted"}, favorites: 1,
			attrs: map[string]any{"format": "doujin"}, popularity: `{"score":{"7d":4}}`, versions: []ver{{langs: []string{"ja"}}}, groups: []string{"yaoi", "saga"}},
		item{n: 4, titles: map[string]string{"en": "Delta"}, age: time.Hour, versions: []ver{{langs: []string{"en"}, future: true}}},
		item{n: 5, titles: map[string]string{"en": "Echo"}, age: time.Hour, listing: "unlisted", versions: []ver{{langs: []string{"en"}}}},
		item{n: 6, titles: map[string]string{"en": "Foxtrot"}, age: time.Hour, deleted: true, versions: []ver{{langs: []string{"en"}}}},
		item{n: 7, kind: "video", titles: map[string]string{"en": "Golf"}, age: 12 * time.Hour, versions: []ver{{langs: []string{"en", "ja"}}}},
	)
	f.exec(`UPDATE {s}.content_memberships SET position = CASE content_id WHEN $1 THEN 2 ELSE 1 END WHERE group_id = $2`, f.cid(1), f.ids["saga"])
	return f
}

func TestListShowsOneCardPerContentUnderVisibility(t *testing.T) {
	f := seeded(t)
	const host = "main.test"
	got, page := f.list(host, "", q("/contents", "kind", "gallery"))
	if want := []string{f.cid(3), f.cid(1)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newest gallery list = %v, want %v (drafts, unlisted, deleted and default-hidden content left out)", got, want)
	}
	alpha := page.Contents[1]
	if alpha.Title != "Alpha" || alpha.Language != "en" || alpha.Version.ID != f.vid(1, 0) || alpha.Code != f.codes[1] || alpha.Path != "/g/"+f.codes[1]+"/alpha" {
		t.Fatalf("card: %+v", alpha)
	}
	if !alpha.Access.Allowed || alpha.Counts.Favorites != 5 || alpha.Counts.Views != 40 || alpha.Version.Pages == nil || *alpha.Version.Pages != 20 {
		t.Fatalf("card access and counts: %+v", alpha)
	}
	if alpha.Attributes["format"] != "manga" || alpha.PublishedAt == nil || alpha.Viewer != nil {
		t.Fatalf("card attributes and anonymous state: %+v", alpha)
	}
	if len(alpha.Groups["tag"]) != 1 || alpha.Groups["tag"][0].Name != "Romance" || alpha.Groups["artist"][0].Slug != "alice" || len(alpha.Groups["series"]) != 1 {
		t.Fatalf("card groups: %+v", alpha.Groups)
	}
	if _, rail := alpha.Groups["rail"]; rail {
		t.Fatal("a group kind without pages is named on cards")
	}
	_, page = f.list(host, "", q("/contents", "kind", "gallery", "card_groups", "artist"))
	if g := page.Contents[1].Groups; len(g) != 1 || len(g["artist"]) != 1 {
		t.Fatalf("card_groups: %+v", g)
	}

	t.Run("display language picks the version and text", func(t *testing.T) {
		_, page := f.list(host, "", q("/contents", "kind", "gallery", "display_language", "ja"))
		alpha := page.Contents[1]
		if alpha.Version.ID != f.vid(1, 1) || alpha.Title != "アルファ" || alpha.Language != "ja" || alpha.Path != "/ja/g/"+f.codes[1]+"/alpha" {
			t.Fatalf("ja card: %+v", alpha)
		}
	})
	t.Run("filters", func(t *testing.T) {
		cases := []struct {
			path string
			want []string
		}{
			{q("/contents", "kind", "gallery", "language", "en"), []string{f.cid(1)}},
			{q("/contents", "kind", "gallery", "language", "ja"), []string{f.cid(3), f.cid(1)}},
			{q("/contents", "kind", "gallery", "traits", "colored"), []string{f.cid(1)}},
			{q("/contents", "kind", "gallery", "groups", f.ids["romance"]), []string{f.cid(1)}},
			{q("/contents", "kind", "gallery", "groups", f.ids["romance"]+","+f.ids["alice"]), []string{f.cid(1)}},
			{q("/contents", "kind", "gallery", "exclude_groups", f.ids["romance"]), []string{f.cid(3)}},
			{q("/contents", "kind", "gallery", "attribute", "format:doujin"), []string{f.cid(3)}},
			{q("/contents", "kind", "gallery", "released_on", f.now.Add(-72*time.Hour).Format(time.DateOnly)), []string{f.cid(1)}},
			{q("/contents", "kind", "gallery", "released_on", "1999-01"), []string{}},
			{q("/contents", "kind", "video"), []string{f.cid(7)}},
			{q("/contents"), []string{f.cid(7), f.cid(3), f.cid(1)}},
		}
		for _, c := range cases {
			if got, _ := f.list(host, "", c.path); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s = %v, want %v", c.path, got, c.want)
			}
		}
		for _, bad := range []string{
			q("/contents", "kind", "comic"), q("/contents", "kind", "gallery", "traits", "dubbed"), q("/contents", "attribute", "format:manga"),
			q("/contents", "kind", "gallery", "attribute", "format:novel"), q("/contents", "released_on", "yesterday"), q("/contents", "sort", "loudest"),
			q("/contents", "sort", "newest:7d"), q("/contents", "sort", "popular:2d"), q("/contents", "sort", "position"), q("/contents", "mode", "peek"),
			q("/contents", "language", "fr"), q("/contents", "view_status", "unseen"), q("/contents", "cursor", "nope"),
		} {
			if status, code := f.call(http.MethodGet, host, "", bad, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
				t.Errorf("%s: %d %s", bad, status, code)
			}
		}
		if status, code := f.call(http.MethodGet, host, "reader", q("/contents", "view_status", "unseen"), nil, nil); status != http.StatusNotImplemented || code != "not_configured" {
			t.Errorf("view_status without a signal plane: %d %s", status, code)
		}
	})
	t.Run("site scopes", func(t *testing.T) {
		if got, _ := f.list("gay.test", "", q("/contents", "kind", "gallery")); !reflect.DeepEqual(got, []string{f.cid(3)}) {
			t.Errorf("a site requiring yaoi = %v", got)
		}
		if got, _ := f.list("clean.test:8080", "", q("/contents", "kind", "gallery")); !reflect.DeepEqual(got, []string{f.cid(1)}) {
			t.Errorf("a site excluding restricted = %v", got)
		}
	})
	t.Run("sorts", func(t *testing.T) {
		cases := map[string][]string{
			"oldest":         {f.cid(1), f.cid(3)},
			"title":          {f.cid(1), f.cid(3)},
			"most_favorited": {f.cid(1), f.cid(3)},
			"popular:7d":     {f.cid(3), f.cid(1)},
			"popular":        {f.cid(1), f.cid(3)},
			"top_rated":      {f.cid(3), f.cid(1)},
		}
		for sort, want := range cases {
			if got, _ := f.list(host, "", q("/contents", "kind", "gallery", "sort", sort)); !reflect.DeepEqual(got, want) {
				t.Errorf("sort %s = %v, want %v", sort, got, want)
			}
		}
		if got, _ := f.list(host, "", q("/contents", "kind", "gallery", "sort", "position", "groups", f.ids["saga"])); !reflect.DeepEqual(got, []string{f.cid(3), f.cid(1)}) {
			t.Errorf("position = %v", got)
		}
	})
	t.Run("cursor pages every sort exactly once", func(t *testing.T) {
		for _, sort := range []string{"newest", "oldest", "title", "popular:7d", "most_favorited", "random", "top_rated:30d"} {
			var seen []string
			path := q("/contents", "sort", sort, "limit", "1")
			for range 5 {
				ids, page := f.list(host, "", path)
				seen = append(seen, ids...)
				if page.NextCursor == "" {
					break
				}
				path = q("/contents", "sort", sort, "limit", "1", "cursor", page.NextCursor)
			}
			slices.Sort(seen)
			if want := []string{f.cid(1), f.cid(3), f.cid(7)}; !reflect.DeepEqual(seen, want) {
				t.Errorf("sort %s paged %v, want %v", sort, seen, want)
			}
		}
	})
}

func TestContentFiltersApplyToEveryRead(t *testing.T) {
	f := seeded(t)
	const host = "main.test"
	var filters ContentFilters
	if status, _ := f.call(http.MethodGet, host, "plain", "/me/content-filters", nil, &filters); status != http.StatusOK {
		t.Fatalf("GET content filters: %d", status)
	}
	if len(filters.Filters) != 2 || filters.Filters[0].Label != "Big breasts" || filters.Filters[0].Visible || filters.Filters[0].Overridden ||
		filters.Filters[1].Label != "Yaoi" || !filters.Filters[1].Visible || filters.IncludeDeleted {
		t.Fatalf("defaults: %+v", filters)
	}
	if status, _ := f.call(http.MethodGet, host, "", "/me/content-filters", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous content filters: %d", status)
	}
	for _, bad := range []ContentFiltersUpdate{
		{Filters: []FilterSetting{{GroupID: f.ids["romance"], Visible: false}}},
		{Filters: []FilterSetting{{GroupID: f.ids["yaoi"]}, {GroupID: f.ids["yaoi"], Visible: true}}},
	} {
		if status, code := f.call(http.MethodPut, host, "plain", "/me/content-filters", bad, nil); status != http.StatusBadRequest {
			t.Fatalf("PUT %+v: %d %s", bad, status, code)
		}
	}
	show := ContentFiltersUpdate{Filters: []FilterSetting{{GroupID: f.ids["big-breasts"], Visible: true}, {GroupID: f.ids["yaoi"], Visible: false}}}
	for _, user := range []string{"plain", "fan"} {
		if status, _ := f.call(http.MethodPut, host, user, "/me/content-filters", show, &filters); status != http.StatusOK {
			t.Fatalf("PUT: %d", status)
		}
	}
	if !filters.Filters[0].Visible || !filters.Filters[0].Overridden || filters.Filters[1].Visible || !filters.Filters[1].Overridden {
		t.Fatalf("after PUT: %+v", filters)
	}
	got, page := f.list(host, "plain", q("/contents", "kind", "gallery"))
	if !reflect.DeepEqual(got, []string{f.cid(2), f.cid(1)}) {
		t.Fatalf("shown big-breasts, hidden yaoi = %v", got)
	}
	bravo := page.Contents[0]
	if bravo.Access.Allowed || !slices.Contains(bravo.Access.Sell, "premium") || bravo.Access.Preview == nil || bravo.Access.Preview.Pages != 3 {
		t.Fatalf("locked card: %+v", bravo.Access)
	}
	if bravo.Viewer == nil || bravo.Viewer.Favorited {
		t.Fatalf("viewer state: %+v", bravo.Viewer)
	}
	if got, _ := f.list(host, "plain", q("/contents", "kind", "gallery", "mode", "hide")); !reflect.DeepEqual(got, []string{f.cid(1)}) {
		t.Fatalf("hide mode = %v", got)
	}
	_, page = f.list(host, "fan", q("/contents", "kind", "gallery", "mode", "hide"))
	if len(page.Contents) != 2 || !page.Contents[0].Access.Allowed || len(page.Contents[0].Access.Sell) != 0 {
		t.Fatalf("a premium holder opens it: %+v", page.Contents)
	}
	f.exec(`INSERT INTO {s}.content_favorites (tenant_id, user_id, content_kind, content_id, value, revision) VALUES ($1, 'plain', 'gallery', $2, 1, 1)`, tenant, f.cid(1))
	if _, page := f.list(host, "plain", q("/contents", "kind", "gallery")); !page.Contents[1].Viewer.Favorited || page.Contents[0].Viewer.Favorited {
		t.Fatalf("favorited: %+v %+v", page.Contents[0].Viewer, page.Contents[1].Viewer)
	}
}

func TestReleaseDates(t *testing.T) {
	f := seeded(t)
	var dates ReleaseDates
	if status, _ := f.call(http.MethodGet, "main.test", "", q("/release-dates", "by", "day", "kind", "gallery"), nil, &dates); status != http.StatusOK {
		t.Fatalf("release dates: %d", status)
	}
	want := []DateCount{{Date: f.now.Add(-24 * time.Hour).Format(time.DateOnly), Count: 1, Free: 1}, {Date: f.now.Add(-72 * time.Hour).Format(time.DateOnly), Count: 1, Free: 1}}
	if !reflect.DeepEqual(dates.Dates, want) || dates.NextCursor != "" {
		t.Fatalf("days = %+v, want %+v", dates, want)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "plain", "/release-dates?by=day&limit=1", nil, &dates); status != http.StatusOK || len(dates.Dates) != 1 || dates.NextCursor == "" {
		t.Fatalf("first page: %+v", dates)
	}
	var rest ReleaseDates
	if status, _ := f.call(http.MethodGet, "main.test", "plain", q("/release-dates", "by", "day", "cursor", dates.NextCursor), nil, &rest); status != http.StatusOK {
		t.Fatal(status)
	}
	if len(rest.Dates) == 0 || rest.Dates[0].Date >= dates.Dates[0].Date {
		t.Fatalf("next page: %+v after %+v", rest, dates)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "fan", "/release-dates", nil, &dates); status != http.StatusOK || len(dates.Dates) == 0 || len(dates.Dates[0].Date) != 7 {
		t.Fatalf("months: %+v", dates)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "", "/release-dates?by=week", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("by week: %d", status)
	}
}

func TestSearchAndTypeahead(t *testing.T) {
	f := seeded(t)
	const host = "main.test"
	f.docs(doc("gallery", f.cid(1), "en", "Alpha Saga"), doc("gallery", f.cid(1), "ja", "アルファ"), doc("gallery", f.cid(2), "en", "Bravo Saga"),
		doc("gallery", f.cid(3), "ja", "Charlie Saga"), doc("gallery", f.cid(5), "en", "Echo Saga"), doc("video", f.cid(7), "en", "Golf Saga"),
		doc("series", f.ids["saga"], "en", "Saga"), doc("tag", f.ids["romance"], "en", "Romance"))
	got, page := f.list(host, "", q("/search", "q", "saga", "kind", "gallery"))
	if !reflect.DeepEqual(got, []string{f.cid(1)}) || page.Contents[0].Title != "Alpha" {
		t.Fatalf("search = %v (hidden, unlisted, other-language and other-kind matches left out)", got)
	}
	if got, _ := f.list(host, "", q("/search", "q", "saga", "display_language", "ja")); !reflect.DeepEqual(got, []string{f.cid(1), f.cid(3), f.cid(7)}) && !slices.Equal(sorted(got), sorted([]string{f.cid(1), f.cid(3), f.cid(7)})) {
		t.Fatalf("ja search = %v", got)
	}
	if got, _ := f.list(host, "", q("/search", "q", "saga", "display_language", "ja", "sort", "newest")); !reflect.DeepEqual(got, []string{f.cid(7), f.cid(3), f.cid(1)}) {
		t.Fatalf("search sorted newest = %v", got)
	}
	if got, _ := f.list(host, "", q("/search", "q", "saga", "display_language", "ja", "exclude_groups", f.ids["saga"])); !reflect.DeepEqual(got, []string{f.cid(7)}) {
		t.Fatalf("filtered search = %v", got)
	}
	var seen []string
	path := q("/search", "q", "saga", "display_language", "ja", "limit", "1")
	for range 5 {
		ids, page := f.list(host, "", path)
		seen = append(seen, ids...)
		if page.NextCursor == "" {
			break
		}
		path = q("/search", "q", "saga", "display_language", "ja", "limit", "1", "cursor", page.NextCursor)
	}
	if !reflect.DeepEqual(sorted(seen), sorted([]string{f.cid(1), f.cid(3), f.cid(7)})) {
		t.Fatalf("paged search = %v", seen)
	}
	if status, _ := f.call(http.MethodGet, host, "", "/search", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("search without q: %d", status)
	}
	if got, _ := f.list("gay.test", "", q("/search", "q", "saga", "display_language", "ja")); !reflect.DeepEqual(got, []string{f.cid(3)}) {
		t.Fatalf("search on a scoped site = %v", got)
	}

	var ta Typeahead
	if status, _ := f.call(http.MethodGet, host, "", q("/typeahead", "q", "sag"), nil, &ta); status != http.StatusOK {
		t.Fatalf("typeahead: %d", status)
	}
	var contents []string
	for _, c := range ta.Contents {
		contents = append(contents, c.ID)
	}
	if !reflect.DeepEqual(sorted(contents), sorted([]string{f.cid(1), f.cid(7)})) || len(ta.Groups) != 1 || string(ta.Groups[0].ID) != f.ids["saga"] {
		t.Fatalf("typeahead = %v %+v", contents, ta.Groups)
	}
	if status, _ := f.call(http.MethodGet, host, "", q("/typeahead", "q", "rom", "group_kinds", "tag"), nil, &ta); status != http.StatusOK || len(ta.Groups) != 1 || ta.Groups[0].Slug != "romance" {
		t.Fatalf("tag typeahead: %+v", ta)
	}
}

func TestSignalPlaneReadsNeedOne(t *testing.T) {
	f := seeded(t)
	for _, c := range []struct{ method, user, path string }{
		{http.MethodGet, "", "/contents/" + f.cid(1) + "/similar"},
		{http.MethodGet, "reader", "/recommendations"},
		{http.MethodGet, "reader", "/me/history"},
		{http.MethodDelete, "reader", "/me/history"},
	} {
		if status, code := f.call(c.method, "main.test", c.user, c.path, nil, nil); status != http.StatusNotImplemented || code != "not_configured" {
			t.Errorf("%s %s: %d %s", c.method, c.path, status, code)
		}
	}
	body := Signals{Session: "s1", Signals: []SignalInput{{ContentID: f.cid(1), Type: TypeView, EventID: "e1", OccurredAt: time.Now()}}}
	if status, code := f.call(http.MethodPost, "main.test", "", "/signals", body, nil); status != http.StatusNotImplemented || code != "not_configured" {
		t.Errorf("signals: %d %s", status, code)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "", "/recommendations", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous recommendations: %d", status)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
