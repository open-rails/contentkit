package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func releaseDay(t *testing.T, out DailyReleases, date string) ReleaseDay {
	t.Helper()
	for _, day := range out.Days {
		if day.Date == date {
			return day
		}
	}
	t.Fatalf("missing date %s", date)
	return ReleaseDay{}
}

func filteredCounts(day ReleaseDay) map[string]int {
	out := map[string]int{}
	for _, entity := range day.FilteredEntities {
		out[entity.Entity.Slug] = entity.Count
	}
	return out
}

func TestDailyReleasesFilteredCountsOverlapAndPreferences(t *testing.T) {
	cfg := siteConfig()
	tag := cfg.EntityKinds["tag"]
	tag.VersionLevel = true
	cfg.EntityKinds["tag"] = tag
	policy := &cfg.ContentFilters.Domains[0]
	policy.Filters = append(policy.Filters, kinds.ContentFilter{Entity: kinds.EntityRef{Kind: "tag", Slug: "unused"}})
	f := newFixtureConfig(t, nil, "", cfg)
	f.now, _ = time.Parse(time.RFC3339, "2026-10-16T02:00:00Z")
	f.clock = f.now
	f.svc.o.Now = func() time.Time { return f.now }
	f.entity("tag", "big-breasts", "yaoi", "unused")
	for i, tags := range [][]string{nil, {"big-breasts"}, {"yaoi"}, {"big-breasts", "yaoi"}} {
		traits := []string{}
		if i == 1 {
			traits = []string{"colored"}
		}
		f.seed(item{n: i + 1, titles: map[string]string{"en": "Story"}, age: time.Hour, entities: tags, versions: []ver{{langs: []string{"en"}, traits: traits}}})
	}
	f.seed(
		item{n: 5, titles: map[string]string{"en": "Two editions"}, age: time.Hour, entities: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 6, titles: map[string]string{"en": "Alternative visible edition"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}},
		item{n: 7, titles: map[string]string{"en": "Only filtered yesterday"}, age: 22 * time.Hour, entities: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, kind: "video", titles: map[string]string{"en": "Other kind"}, age: time.Hour, entities: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
	)
	if err := f.entities.AddMemberships(f.ctx, []entities.Membership{{EntityID: entities.EntityID(f.ids["big-breasts"]), ContentID: f.cid(6), VersionID: f.vid(6, 0)}}); err != nil {
		t.Fatal(err)
	}
	choices := ContentFiltersUpdate{Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: false}, FilterSetting{EntityID: f.ids["yaoi"], Visible: false})}
	raw, _ := json.Marshal(choices.Filters)
	header := `{"filters":` + string(raw) + `}`
	read := func(user, language, guest string) DailyReleases {
		t.Helper()
		var out DailyReleases
		w := f.guestCall(http.MethodGet, "main.test", user, q("/releases", "kind", "gallery", "language", language, "time_zone", "America/Denver"), guest, nil, &out)
		if w.Code != http.StatusOK {
			t.Fatalf("releases: %d %s", w.Code, w.Body)
		}
		return out
	}
	assertDay := func(day ReleaseDay, total, big, yaoi, visible int) {
		t.Helper()
		counts := filteredCounts(day)
		if day.FilteredCount != total || counts["big-breasts"] != big || counts["yaoi"] != yaoi || counts["unused"] != 0 || len(day.Contents) != visible {
			t.Fatalf("day %+v counts %v want total=%d big=%d yaoi=%d cards=%d", day, counts, total, big, yaoi, visible)
		}
		if _, ok := counts["unused"]; !ok {
			t.Fatal("zero-count hidden toggle omitted")
		}

	}
	// A content with two hidden editions counts once; intersecting tags overlap.
	out := read("", "en", header)
	assertDay(releaseDay(t, out, "2026-10-15"), 5, 4, 2, 1)
	assertDay(releaseDay(t, out, "2026-10-14"), 1, 1, 0, 0)
	if len(out.Days) != 10 || len(releaseDay(t, out, "2026-10-13").Contents) != 0 {
		t.Fatal("calendar extent changed")
	}
	// No language restriction permits the alternative unfiltered edition. It
	// must not count again just because the preferred English edition is hidden.
	assertDay(releaseDay(t, read("", "", header), "2026-10-15"), 4, 3, 2, 2)
	assertDay(releaseDay(t, read("", "ja", header), "2026-10-15"), 1, 1, 0, 1)
	for _, tc := range []struct {
		params                    []string
		total, big, yaoi, visible int
	}{
		{[]string{"traits", "colored"}, 1, 1, 0, 0},
		{[]string{"exclude_groups", f.ids["big-breasts"]}, 1, 0, 1, 1},
		{[]string{"groups", f.ids["big-breasts"]}, 1, 0, 1, 3},
	} {
		var selection DailyReleases
		params := append([]string{"kind", "gallery", "language", "en", "time_zone", "America/Denver"}, tc.params...)
		if w := f.guestCall(http.MethodGet, "main.test", "", q("/releases", params...), header, nil, &selection); w.Code != http.StatusOK {
			t.Fatalf("filtered selection: %d %s", w.Code, w.Body)
		}
		assertDay(releaseDay(t, selection, "2026-10-15"), tc.total, tc.big, tc.yaoi, tc.visible)
	}
	var mixed DailyReleases
	if w := f.guestCall(http.MethodGet, "main.test", "", q("/releases", "language", "en", "time_zone", "America/Denver"), header, nil, &mixed); w.Code != http.StatusOK {
		t.Fatalf("mixed kinds: %d %s", w.Code, w.Body)
	}
	mixedDay := releaseDay(t, mixed, "2026-10-15")
	if mixedDay.FilteredCount != 6 || filteredCounts(mixedDay)["big-breasts"] != 5 {
		t.Fatalf("mixed kind counts: %+v", mixedDay)
	}
	// Account preferences are authoritative; changing them immediately changes
	// the next release response, and a forged guest header cannot replace them.
	if status, _ := f.call(http.MethodPatch, "main.test", "reader", "/me/content-filters", choices, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	assertDay(releaseDay(t, read("reader", "en", ""), "2026-10-15"), 5, 4, 2, 1)
	choices.Filters[0].Visible = boolPtr(true)
	if status, _ := f.call(http.MethodPatch, "main.test", "reader", "/me/content-filters", choices, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	assertDay(releaseDay(t, read("reader", "en", header), "2026-10-15"), 2, 0, 2, 4)
}

func TestDailyReleasesFilteredCountsKeepHardAndAccessPolicies(t *testing.T) {
	cfg := siteConfig()
	no := false
	cfg.ContentFilters.Domains, cfg.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", Hosts: []string{"required.test"}, RequireEntities: map[string][]kinds.EntityRef{"gallery": []kinds.EntityRef{{Kind: "tag", Slug: "yaoi"}}}, Filters: []kinds.ContentFilter{
		{Entity: kinds.EntityRef{Kind: "tag", Slug: "big-breasts"}},
		{Entity: kinds.EntityRef{Kind: "tag", Slug: "yaoi"}, Visible: true},
		{Entity: kinds.EntityRef{Kind: "tag", Slug: "gore"}, UserToggle: &no},
		{Entity: kinds.EntityRef{Kind: "tag", Slug: "restricted"}, UserToggle: &no},
	},
	}}, []string{"gallery"}
	f := newFixtureConfig(t, nil, "", cfg)
	f.entity("tag", "big-breasts", "yaoi", "gore")
	for n, extra := range map[int][]string{1: nil, 2: {"gore"}, 3: {"restricted"}} {
		f.seed(item{n: n, titles: map[string]string{"en": "PRIVATE_OMITTED"}, age: time.Hour, entities: append([]string{"big-breasts", "yaoi"}, extra...), versions: []ver{{langs: []string{"en"}}}})
	}
	f.seed(
		item{n: 4, titles: map[string]string{"en": "Outside required domain"}, age: time.Hour, entities: []string{"big-breasts"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 5, titles: map[string]string{"en": "Future"}, age: time.Hour, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}, future: true}}},
		item{n: 6, titles: map[string]string{"en": "Draft"}, age: time.Hour, draft: true, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 7, titles: map[string]string{"en": "Deleted"}, age: time.Hour, deleted: true, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 8, titles: map[string]string{"en": "Premium"}, age: time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 9, titles: map[string]string{"en": "Members only"}, age: time.Hour, rule: `{"level":"tier","tiers":["premium"]}`, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"en"}}}},
		item{n: 10, titles: map[string]string{"ja": "Other language"}, age: time.Hour, entities: []string{"big-breasts", "yaoi"}, versions: []ver{{langs: []string{"ja"}}}},
	)
	if status, _ := f.call(http.MethodPatch, "required.test", "fan", "/me/content-filters", ContentFiltersUpdate{IncludeDeleted: boolPtr(true)}, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	for _, tc := range []struct {
		user, mode     string
		total, visible int
	}{{"", "lock", 3, 0}, {"", "hide", 1, 0}, {"fan", "hide", 3, 0}, {"staff", "lock", 3, 0}} {
		var out DailyReleases
		if status, code := f.call(http.MethodGet, "required.test", tc.user, q("/releases", "kind", "gallery", "language", "en", "mode", tc.mode), nil, &out); status != http.StatusOK {
			t.Fatalf("%+v: %d %s", tc, status, code)
		}
		total, visible := 0, 0
		for _, day := range out.Days {
			total += day.FilteredCount
			visible += len(day.Contents)
			for _, entry := range day.FilteredEntities {
				if entry.Entity.Slug != "big-breasts" || entry.Entity.Internal || entry.Count != day.FilteredCount {
					t.Fatalf("hard/internal/required entity leaked: %+v", entry)
				}
			}
		}
		if total != tc.total || visible != tc.visible {
			t.Fatalf("%+v: total=%d visible=%d", tc, total, visible)
		}
		if tc.user != "staff" {
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "PRIVATE_OMITTED") || strings.Contains(string(raw), f.cid(1)) || strings.Contains(string(raw), f.ids["restricted"]) {
				t.Fatal("omitted content or internal marker metadata leaked")
			}
		}
	}
}

type releaseMedia struct{ calls int }

func (m *releaseMedia) Covers(_ context.Context, picks []versions.CoverOf) ([][]media.PublicRendition, error) {
	m.calls += len(picks)
	return make([][]media.PublicRendition, len(picks)), nil
}
func (*releaseMedia) Ladder(string) string { return "" }

func TestDailyReleasesFilteredCountsRespectLockedEntityRedaction(t *testing.T) {
	cfg := siteConfig()
	kind := cfg.Kinds["gallery"]
	kind.Locked = &kinds.Locked{Show: []string{kinds.ShowCover}}
	cfg.Kinds["gallery"] = kind
	f := newFixtureConfig(t, nil, "", cfg)
	f.entity("tag", "big-breasts")
	f.seed(item{n: 1, titles: map[string]string{"en": "Private title"}, age: time.Hour, entities: []string{"big-breasts"}, rule: `{"level":"tier","tiers":["premium"]}`, versions: []ver{{langs: []string{"en"}}}})
	m := &releaseMedia{}
	f.svc.o.Media = m
	for _, tc := range []struct {
		user        string
		entityCount int
	}{{"", 0}, {"fan", 1}} {
		var out DailyReleases
		if status, _ := f.call(http.MethodGet, "main.test", tc.user, "/releases?kind=gallery&language=en", nil, &out); status != http.StatusOK {
			t.Fatal(status)
		}
		total, perEntity := 0, 0
		for _, day := range out.Days {
			total += day.FilteredCount
			perEntity += filteredCounts(day)["big-breasts"]
		}
		if total != 1 || perEntity != tc.entityCount || m.calls != 0 {
			t.Fatalf("as %s: total=%d entities=%d cover calls=%d", tc.user, total, perEntity, m.calls)
		}
	}
}

type releaseBilling struct {
	access.Billing
	queries []access.Query
	held    map[string]bool
}

func (b *releaseBilling) Held(_ context.Context, _ string, q access.Query) (access.Answer, error) {
	b.queries = append(b.queries, q)
	out := access.Answer{Keys: map[string]bool{}, Held: map[string]access.HeldKeys{}}
	for _, key := range q.Keys {
		out.Keys[key] = b.held[key]
	}
	for _, prefix := range q.Prefixes {
		out.Held[prefix] = access.HeldKeys{Truncated: true}
	}
	return out, nil
}

func TestDailyReleasesFilteredCountsSettleTruncatedEntitlements(t *testing.T) {
	billing := &releaseBilling{held: map[string]bool{}}
	f := newFixtureBilling(t, nil, "", siteConfig(), billing)
	f.entity("tag", "big-breasts")
	f.seed(
		item{n: 1, titles: map[string]string{"en": "Owned hidden"}, age: time.Hour, entities: []string{"big-breasts"}, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, titles: map[string]string{"en": "Unowned hidden"}, age: time.Hour, entities: []string{"big-breasts"}, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, titles: map[string]string{"en": "Members hidden"}, age: time.Hour, entities: []string{"big-breasts"}, rule: `{"level":"ppv"}`, versions: []ver{{langs: []string{"en"}}}},
	)
	billing.held[tenant+":content:"+f.cid(1)] = true
	for _, tc := range []struct {
		mode  string
		total int
	}{{"hide", 1}, {"lock", 3}} {
		billing.queries = nil
		out, err := f.svc.Releases(f.ctx, DailyReleasesOptions{Reader: Reader{Actor: access.Actor{ID: "reader"}, Host: "main.test", Language: "en", Mode: access.Mode(tc.mode)}, Filter: Filter{Kind: "gallery"}})
		if err != nil {
			t.Fatal(err)
		}
		total, prefixes := 0, 0
		for _, day := range out.Days {
			total += day.FilteredCount
		}
		for _, q := range billing.queries {
			if len(q.Prefixes) > 0 {
				prefixes++
			}
		}
		if total != tc.total || prefixes != 1 || len(billing.queries) != 2 {
			t.Fatalf("%s: filtered=%d queries=%+v", tc.mode, total, billing.queries)
		}
		// Tier holdings are memoized while explicit content keys are settled.
		if slices.Contains(billing.queries[1].Keys, "premium") {
			t.Fatal("resolved tier was read again")
		}
	}
}
