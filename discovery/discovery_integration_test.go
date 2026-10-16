package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
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
			attrs: map[string]any{"format": "doujin"}, popularity: `{"score":{"7d":4}}`, viewBaseline: 100, versions: []ver{{langs: []string{"ja"}}}, groups: []string{"yaoi", "saga"}},
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
	if alpha.Title != "Alpha" || alpha.Language != "en" || alpha.Version.ID != f.vid(1, 0) || alpha.Code != f.codes[1] || alpha.Path != "/en/g/"+f.codes[1]+"/alpha" {
		t.Fatalf("card: %+v", alpha)
	}
	if !alpha.Access.Allowed || *alpha.Counts.Favorites != 5 || *alpha.Counts.Views != 40 || alpha.Version.Pages == nil || *alpha.Version.Pages != 20 {
		t.Fatalf("card access and counts: %+v", alpha)
	}
	if views := page.Contents[0].Counts.Views; *views != 100 {
		t.Fatalf("an imported view baseline counts as views: %d", *views)
	}
	v, err := f.cat.Viewer(f.ctx, catalog.ViewerOptions{Actor: access.Actor{Anonymous: true}, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if items, err := f.cat.Contents(f.ctx, v, []string{f.cid(1), f.cid(3)}); err != nil || *items[0].Views != 40 || *items[1].Views != 100 {
		t.Fatalf("detail views %v: viewers %+v, baseline %+v", err, items[0], items[1])
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
	_, page = f.list(host, "", q("/contents", "kind", "gallery", "card_entities", "artist"))
	if g := page.Contents[1].Groups; len(g) != 1 || len(g["artist"]) != 1 {
		t.Fatalf("card_entities: %+v", g)
	}

	t.Run("display language picks the version and text", func(t *testing.T) {
		_, page := f.list(host, "", q("/contents", "kind", "gallery", "display_language", "ja"))
		alpha := page.Contents[1]
		if alpha.Version.ID != f.vid(1, 1) || alpha.Title != "アルファ" || alpha.Language != "ja" || alpha.Path != "/ja/g/"+f.codes[1]+"/alpha" {
			t.Fatalf("ja card: %+v", alpha)
		}
		_, page = f.list(host, "", q("/contents", "kind", "gallery", "display_language", "en"))
		if p := page.Contents[1].Path; p != "/en/g/"+f.codes[1]+"/alpha" {
			t.Fatalf("the default language is prefixed too: %s", p)
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
			q("/contents", "sort", "newest:7d"), q("/contents", "sort", "most_viewed:7d"), q("/contents", "sort", "popular:2d"), q("/contents", "sort", "position"), q("/contents", "mode", "peek"),
			q("/contents", "sort", "position", "groups", f.ids["romance"]),
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
			"popular":        {f.cid(1), f.cid(3)}, // a view baseline is no popularity
			"most_viewed":    {f.cid(3), f.cid(1)},
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
		for _, sort := range []string{"newest", "oldest", "title", "popular:7d", "most_favorited", "most_viewed", "random", "top_rated:30d"} {
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
	if status, _ := f.call(http.MethodGet, host, "plain", "/content-filters", nil, &filters); status != http.StatusOK {
		t.Fatalf("GET content filters: %d", status)
	}
	if len(filters.Filters) != 2 || filters.Filters[0].Label != "Big breasts" || filters.Filters[0].Visible || filters.Filters[0].Overridden ||
		filters.Filters[1].Label != "Yaoi" || !filters.Filters[1].Visible || filters.IncludeDeleted {
		t.Fatalf("defaults: %+v", filters)
	}
	if status, _ := f.call(http.MethodGet, host, "", "/content-filters", nil, nil); status != http.StatusOK {
		t.Fatalf("anonymous content filters: %d", status)
	}
	// A hidden group's content stays out of other lists, but its own page
	// shows what its count counts.
	if err := f.groups.Recount(f.ctx); err != nil {
		t.Fatal(err)
	}
	dir, err := f.groups.List(f.ctx, groups.ListOptions{Kind: "tag", Slug: "big-breasts"})
	if err != nil || len(dir.Groups) != 1 {
		t.Fatalf("directory: %+v %v", dir, err)
	}
	for _, user := range []string{"", "plain"} {
		if got, _ := f.list(host, user, q("/contents", "kind", "gallery")); slices.Contains(got, f.cid(2)) {
			t.Fatalf("a default-hidden tag listed for %q: %v", user, got)
		}
		if got, _ := f.list(host, user, q("/contents", "groups", f.ids["big-breasts"])); len(got) != dir.Groups[0].Count || !reflect.DeepEqual(got, []string{f.cid(2)}) {
			t.Fatalf("the hidden tag's page for %q = %v, its count %d", user, got, dir.Groups[0].Count)
		}
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
	if got, _ := f.list(host, "plain", q("/contents", "groups", f.ids["yaoi"])); !reflect.DeepEqual(got, []string{f.cid(3)}) {
		t.Fatalf("the yaoi page with yaoi hidden = %v", got)
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
	if status, _ := f.call(http.MethodGet, host, "", q("/typeahead", "q", "rom", "entity_kinds", "tag"), nil, &ta); status != http.StatusOK || len(ta.Groups) != 1 || ta.Groups[0].Slug != "romance" {
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

// Lists filter by access (what anyone opens, what stays locked at a level or
// behind a tier) and by credited creator; search shares the filters. Locked
// cards preview the pages their kind's locked view names when their rule
// names none.
func TestListFiltersByAccessAndCreator(t *testing.T) {
	f := newFixture(t, nil, "")
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Free"}, age: 4 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Premium"}, age: 3 * time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Released"}, age: 2 * time.Hour, rule: `{"level":"ppv","free_after":"` + past + `"}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, titles: map[string]string{"en": "Sold"}, age: time.Hour, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
	)
	f.exec(`INSERT INTO {s}.content_creators (tenant_id, content_id, user_id, role) VALUES ($1, $2, 'alice', 'author'), ($1, $3, 'alice', 'artist'), ($1, $3, 'bob', 'author')`,
		tenant, f.cid(2), f.cid(4))
	const host = "main.test"
	for path, want := range map[string][]string{
		q("/contents", "access", "public"):                     {f.cid(3), f.cid(1)},
		q("/contents", "access", "tier"):                       {f.cid(2)},
		q("/contents", "access", "ppv"):                        {f.cid(4)},
		q("/contents", "access", "public,ppv"):                 {f.cid(4), f.cid(3), f.cid(1)},
		q("/contents", "tiers", "premium"):                     {f.cid(2)},
		q("/contents", "access", "public", "tiers", "premium"): {},
		q("/contents", "creators", "alice"):                    {f.cid(4), f.cid(2)},
		q("/contents", "creators", "bob,carol"):                {f.cid(4)},
		q("/contents", "creators", "alice", "access", "tier"):  {f.cid(2)},
		q("/release-dates", "tiers", "premium"):                nil,
	} {
		if strings.HasPrefix(path, "/release-dates") {
			var out ReleaseDates
			if status, code := f.call(http.MethodGet, host, "", path, nil, &out); status != http.StatusOK || len(out.Dates) != 1 || out.Dates[0].Count != 1 {
				t.Errorf("%s: %d %s %+v", path, status, code, out)
			}
			continue
		}
		if got, _ := f.list(host, "", path); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	_, page := f.list(host, "", q("/contents", "kind", "gallery", "sort", "oldest"))
	for i, want := range []int{0, 2, 0, 2} {
		got := 0
		if p := page.Contents[i].Access.Preview; p != nil {
			got = p.Pages
		}
		if got != want || page.Contents[i].Access.Allowed != (want == 0) {
			t.Errorf("card %d previews %d pages (allowed %t), want %d", i+1, got, page.Contents[i].Access.Allowed, want)
		}
	}
	// Every reader sees each card's effective rule, and its version's audio.
	f.exec(`UPDATE {s}.content_versions SET has_audio = true WHERE id = $1`, f.vid(2, 0))
	for _, user := range []string{"", "fan", "staff"} {
		_, page := f.list(host, user, q("/contents", "kind", "gallery", "sort", "oldest"))
		var got []string
		for _, c := range page.Contents {
			got = append(got, fmt.Sprintf("%s%v audio=%t", c.Access.Level, c.Access.Tiers, c.Version.HasAudio))
		}
		if want := []string{"public[] audio=false", "tier[premium] audio=true", "public[] audio=false", "ppv[] audio=false"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%q: cards %v, want %v", user, got, want)
		}
	}
	for _, bad := range []string{q("/contents", "access", "vip"), q("/contents", "tiers", "gold"), q("/search", "q", "x", "tiers", "gold")} {
		if status, code := f.call(http.MethodGet, host, "", bad, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%s: %d %s", bad, status, code)
		}
	}
}

// The sitemap feed pages a site's public pages: live, listed content in its
// scope (content filters, a reader's taste, don't apply), and the groups
// holding it, each with the languages served.
func TestSitemapFeed(t *testing.T) {
	f := seeded(t)
	feed := func(host, path string) SitemapPage {
		t.Helper()
		var page SitemapPage
		if status, code := f.call(http.MethodGet, host, "", path, nil, &page); status != http.StatusOK {
			t.Fatalf("GET %s on %s: %d %s", path, host, status, code)
		}
		return page
	}
	ids := func(p SitemapPage) []string {
		out := []string{}
		for _, e := range p.Entries {
			out = append(out, e.ID)
		}
		return out
	}
	page := feed("main.test", q("/sitemap/contents", "kind", "gallery"))
	if got, want := ids(page), []string{f.cid(1), f.cid(2), f.cid(3)}; !reflect.DeepEqual(got, want) || page.NextCursor != "" {
		t.Fatalf("gallery sitemap = %v, want %v", got, want)
	}
	alpha, charlie := page.Entries[0], page.Entries[2]
	if alpha.Code != f.codes[1] || alpha.Path != "/en/g/"+f.codes[1]+"/alpha" || alpha.LastMod.IsZero() || len(alpha.Languages) != 2 ||
		alpha.Languages[0] != (SitemapAlias{Language: "en", Path: alpha.Path}) || alpha.Languages[1].Language != "ja" ||
		!strings.HasPrefix(alpha.Languages[1].Path, "/ja/g/"+f.codes[1]) {
		t.Fatalf("alpha %+v", alpha)
	}
	if len(charlie.Languages) != 1 || charlie.Path != charlie.Languages[0].Path || !strings.HasPrefix(charlie.Path, "/ja/g/"+f.codes[3]) {
		t.Fatalf("a page only in a second language %+v", charlie)
	}
	if got := ids(feed("clean.test", q("/sitemap/contents", "kind", "gallery"))); !reflect.DeepEqual(got, []string{f.cid(1), f.cid(2)}) {
		t.Errorf("a site excluding restricted = %v", got)
	}
	if got := ids(feed("gay.test", q("/sitemap/contents", "kind", "gallery"))); !reflect.DeepEqual(got, []string{f.cid(3)}) {
		t.Errorf("a site requiring yaoi = %v", got)
	}
	first := feed("main.test", q("/sitemap/contents", "kind", "gallery", "limit", "2"))
	rest := feed("main.test", q("/sitemap/contents", "kind", "gallery", "limit", "2", "cursor", first.NextCursor))
	if got := append(ids(first), ids(rest)...); !reflect.DeepEqual(got, []string{f.cid(1), f.cid(2), f.cid(3)}) || first.NextCursor == "" || rest.NextCursor != "" {
		t.Errorf("paged = %v (%q, %q)", got, first.NextCursor, rest.NextCursor)
	}

	tags := feed("main.test", q("/sitemap/entities", "kind", "tag"))
	if got := ids(tags); !slices.IsSorted(got) || !reflect.DeepEqual(sorted(got), sorted([]string{f.ids["romance"], f.ids["big-breasts"], f.ids["yaoi"]})) {
		t.Fatalf("tag sitemap = %v", got)
	}
	byID := map[string]SitemapEntry{}
	for _, e := range tags.Entries {
		byID[e.ID] = e
	}
	if romance := byID[f.ids["romance"]]; romance.Code == "" || romance.Path != "/en/tags/"+romance.Code+"/romance" || len(romance.Languages) != 2 {
		t.Fatalf("romance %+v", romance)
	}
	if yaoi := byID[f.ids["yaoi"]]; len(yaoi.Languages) != 1 || yaoi.Languages[0].Language != "ja" || yaoi.Path != "/ja/tags/"+yaoi.Code+"/yaoi" {
		t.Fatalf("yaoi %+v", yaoi)
	}
	if got := ids(feed("clean.test", q("/sitemap/entities", "kind", "series"))); !reflect.DeepEqual(got, []string{f.ids["saga"]}) {
		t.Errorf("series on a scoped site = %v", got)
	}
	if got := ids(feed("main.test", q("/sitemap/entities", "kind", "artist"))); !reflect.DeepEqual(got, []string{f.ids["alice"]}) {
		t.Errorf("artists = %v", got)
	}
	for _, bad := range []string{q("/sitemap/contents"), q("/sitemap/contents", "kind", "comic"), q("/sitemap/entities", "kind", "rail"),
		q("/sitemap/entities", "kind", "tag", "cursor", "!"), q("/sitemap/contents", "kind", "gallery", "limit", "0")} {
		if status, code := f.call(http.MethodGet, "main.test", "", bad, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%s: %d %s", bad, status, code)
		}
	}
}

// Staff rank a kind's live, listed content by published comments over a
// period, filtered by access as lists are; readers read their own account
// statistics. View rankings and activity need the signal plane.
func TestStats(t *testing.T) {
	f := newFixture(t, nil, "")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Free"}, age: 4 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Premium"}, age: 3 * time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Hidden"}, age: 2 * time.Hour, listing: "unlisted", versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, kind: "video", titles: map[string]string{"en": "Clip"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
	)
	comment := func(n int, user string, age time.Duration, likes int, extra string) {
		kind := "gallery"
		if n == 4 {
			kind = "video"
		}
		f.exec(`INSERT INTO {s}.content_comments (tenant_id, content_kind, content_id, user_id, body, likes, created_at, deleted_at, moderation)
 VALUES ($1, $2, $3, $4, 'hi', $5, now() - $6::interval, CASE WHEN $7 = 'deleted' THEN now() END, CASE WHEN $7 = 'held' THEN 'held' ELSE 'approved' END)`,
			tenant, kind, f.cid(n), user, likes, fmt.Sprintf("%d seconds", int(age.Seconds())), extra)
	}
	for range 3 {
		comment(2, "ann", time.Hour, 0, "")
	}
	comment(1, "ann", 40*24*time.Hour, 2, "")
	comment(1, "bob", time.Hour, 1, "")
	comment(1, "bob", time.Hour, 0, "deleted")
	comment(1, "bob", time.Hour, 0, "held")
	for range 5 {
		comment(3, "bob", time.Hour, 0, "")
	}
	comment(4, "ann", time.Hour, 0, "")
	const host = "main.test"
	top := func(path string) []RankedContent {
		t.Helper()
		var out TopContents
		if status, code := f.call(http.MethodGet, host, "staff", path, nil, &out); status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, status, code)
		}
		return out.Contents
	}
	got := top(q("/stats/top", "kind", "gallery", "metric", "comments", "period", "all"))
	if want := []RankedContent{{ID: f.cid(2), Title: "Premium", Path: "/en/g/" + f.codes[2] + "/premium", Value: 3}, {ID: f.cid(1), Title: "Free",
		Path: "/en/g/" + f.codes[1] + "/free", Value: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("most commented = %+v, want %+v (unlisted, deleted and held left out)", got, want)
	}
	if got := top(q("/stats/top", "kind", "gallery", "metric", "comments", "period", "30d", "access", "public")); len(got) != 1 || got[0].ID != f.cid(1) || got[0].Value != 1 {
		t.Errorf("free, last 30 days = %+v", got)
	}
	if got := top(q("/stats/top", "kind", "gallery", "metric", "comments", "tiers", "premium", "limit", "1", "language", "ja")); len(got) != 1 ||
		got[0].ID != f.cid(2) || got[0].Path != "/ja/g/"+f.codes[2]+"/premium" {
		t.Errorf("premium = %+v", got)
	}
	for path, want := range map[string]int{
		q("/stats/top", "kind", "gallery", "metric", "comments"):                 http.StatusForbidden,
		q("/stats/top", "kind", "comic", "metric", "comments"):                   http.StatusBadRequest,
		q("/stats/top", "kind", "gallery", "metric", "likes"):                    http.StatusBadRequest,
		q("/stats/top", "kind", "gallery", "metric", "comments", "period", "2d"): http.StatusBadRequest,
		q("/stats/top", "kind", "gallery"):                                       http.StatusNotImplemented,
		q("/stats/activity", "kind", "gallery"):                                  http.StatusNotImplemented,
		q("/stats/activity", "kind", "gallery", "days", "91"):                    http.StatusBadRequest,
	} {
		user := "staff"
		if want == http.StatusForbidden {
			user = "reader"
		}
		if status, _ := f.call(http.MethodGet, host, user, path, nil, nil); status != want {
			t.Errorf("%s as %s: %d, want %d", path, user, status, want)
		}
	}
	var mine MyStats
	if status, code := f.call(http.MethodGet, host, "bob", "/me/stats", nil, &mine); status != http.StatusOK || mine.Comments != 6 || mine.CommentLikes != 1 ||
		mine.CommentDislikes != 0 || len(mine.Completed) != 0 {
		t.Fatalf("bob's stats: %d %s %+v", status, code, mine)
	}
	if status, _ := f.call(http.MethodGet, host, "", "/me/stats", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous stats: %d", status)
	}
}
