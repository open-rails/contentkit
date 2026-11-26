package critical_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

// domainTransport is bearerTransport on another Host.
type domainTransport struct {
	test        testing.TB
	auth        *authkit.Client
	host, token string
}

func (d domainTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Host = d.host
	presentToken(d.test, d.auth, r, d.token)
	return http.DefaultTransport.RoundTrip(r)
}

func assertCards(t *testing.T, page discovery.ContentPage, want ...string) {
	t.Helper()
	got := []string{}
	for _, card := range page.Contents {
		got = append(got, card.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("visible card IDs = %v; want %v", got, want)
	}
}

func TestDiscoveryVisibility(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", MembershipScopes: []kinds.MembershipScope{kinds.ContentScope, kinds.VersionScope}}
		article := cfg.Content.Kinds["article"]
		article.Entities, article.Versions.Hidden, article.Locked = []string{"tag"}, false, &kinds.Locked{Show: []string{}}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	admin := a.engine.Client()
	tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{
		{Kind: "tag", Slug: "required", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Required public tag"}, {Language: "en", Kind: "alias", Name: "MandatoryAliasNeedle"}}},
		{Kind: "tag", Slug: "choice", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Optional public tag"}, {Language: "en", Kind: "alias", Name: "ChoiceNeedle"}}},
		{Kind: "tag", Slug: "internal", Internal: true, Names: []entities.Name{{Language: "en", Kind: "name", Name: "HiddenClassificationNeedle"}}},
		{Kind: "tag", Slug: "elsewhere", Names: []entities.Name{{Language: "en", Kind: "name", Name: "ElsewhereOnlyNeedle"}, {Language: "en", Kind: "alias", Name: "Tow"}}},
	})
	must(t, err)
	if len(tags) != 4 {
		t.Fatal("classification fixture incomplete")
	}
	required, optional, internal, elsewhere := tags[0], tags[1], tags[2], tags[3]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{
		{Name: "main", Hosts: []string{"restricted.invalid"}, RequireEntities: map[string][]kinds.EntityRef{"article": {{Kind: "tag", Slug: "required"}}}, Filters: []kinds.ContentFilter{{Entity: kinds.EntityRef{Kind: "tag", Slug: "choice"}, Visible: true}}},
		{Name: "wide", Hosts: []string{"wide.invalid"}},
	}}
	article := a.cfg.Content.Kinds["article"]
	article.Popularity = []kinds.PopularityBucket{
		{ID: "7d", Days: 7, Scopes: []kinds.PopularityScope{{Domain: "main"}}},
		{ID: "30d", Days: 30, Scopes: []kinds.PopularityScope{{Domain: "main"}}},
		{ID: "november", From: "2025-11-01", Until: "2025-11-24", Scopes: []kinds.PopularityScope{{Domain: "main"}}},
	}
	a.cfg.Content.Kinds["article"] = article
	a.cfg.Database.SkipMigrations = true
	configureDiscoverySignals(t, w, a)
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	startPublicationRuntime(t, a)
	admin = a.engine.Client()
	reader := func(host, token string) *ck.Client {
		return ck.NewClient(a.server.URL+"/content", &http.Client{Transport: domainTransport{test: t, auth: w.auth, host: host, token: token}})
	}
	public, wide := reader("restricted.invalid", ""), reader("wide.invalid", "")
	create := func(title string, membership []entities.EntityID, publishAt *time.Time, publish bool) catalog.Content {
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "CriticalVis " + title, Body: "Private source body " + title}}}}})
		must(t, err)
		links := []entities.Membership{}
		for _, id := range membership {
			links = append(links, entities.Membership{EntityID: id})
		}
		_, err = admin.PutContentEntitiesShared(t.Context(), item.ID, entities.ContentEntities{Memberships: links}, ck.PutContentEntitiesSharedQuery{})
		must(t, err)
		if publish {
			item, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: publishAt})
			must(t, err)
			if publishAt == nil || !publishAt.After(w.clock.now()) {
				// Some fixtures intentionally fail the default domain's entity
				// filter. Observe completion through management, then assert their
				// ordinary reader visibility below using each explicit host.
				waitForWithin(t, 45*time.Second, func() bool {
					wakeDuePublicationJobs(t, a)
					managed, err := admin.GetManage(t.Context(), ck.GetManageQuery{Ids: []string{item.ID}})
					must(t, err)
					if len(managed) != 1 || len(managed[0].Versions) == 0 {
						return false
					}
					for _, version := range managed[0].Versions {
						if !version.Live {
							return false
						}
					}
					return true
				})
			}
		}
		return item
	}
	one := create("allowed one", []entities.EntityID{required.ID, internal.ID}, nil, true)
	two := create("allowed two", []entities.EntityID{required.ID, optional.ID}, nil, true)
	excluded := create("outside domain", []entities.EntityID{optional.ID, elsewhere.ID}, nil, true)
	draft := create("unpublished", []entities.EntityID{required.ID}, nil, false)
	futureAt := w.clock.now().Add(time.Hour)
	future := create("future", []entities.EntityID{required.ID}, &futureAt, true)
	deleted := create("deleted", []entities.EntityID{required.ID}, nil, true)
	if len(deleted.Versions) != 1 {
		t.Fatal("fixture needs one deletable version")
	}
	err = admin.DeleteContentVersion(t.Context(), deleted.ID, deleted.Versions[0].ID)
	must(t, err)
	locked := create("locked", []entities.EntityID{required.ID}, nil, true)
	_, personal := w.person(t, 0)
	viewer := reader("restricted.invalid", personal)
	postComment(t, viewer, locked.ID, "A real restricted comment")
	_, err = viewer.PostContentFavorite(t.Context(), locked.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	standing, err := viewer.GetContentCanComment(t.Context(), locked.ID, ck.GetContentCanCommentQuery{})
	must(t, err)
	if standing.Count == nil || *standing.Count != 1 {
		t.Fatalf("restricted-count positive control absent: %+v", standing)
	}
	_, err = admin.PatchContentVersion(t.Context(), locked.ID, locked.Versions[0].ID, catalog.VersionInput{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}})
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, a)
		time.Sleep(50 * time.Millisecond)
		current, err := public.GetContent(t.Context(), locked.ID, ck.GetContentQuery{})
		return err == nil && !current.Decision.Allowed
	})
	waitForWithin(t, 45*time.Second, func() bool {
		found, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		return len(found.Contents) == 2
	})
	t.Run("CK-J07-S01_locked_detail_hides_body_and_real_interaction_counts", func(t *testing.T) {
		got, err := public.GetContent(t.Context(), locked.ID, ck.GetContentQuery{})
		must(t, err)
		if got.Decision.Allowed || got.Body != "" || len(got.Images) != 0 || len(got.Attributes) != 0 {
			t.Fatalf("locked detail leaked private fields: %+v", got)
		}
		standing, err := viewer.GetContentCanComment(t.Context(), locked.ID, ck.GetContentCanCommentQuery{})
		must(t, err)
		if !standing.Locked || standing.Count != nil || standing.CanComment {
			t.Fatalf("locked standing leaked real count: %+v", standing)
		}
		page, err := public.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{locked.ID}})
		must(t, err)
		assertCards(t, page, locked.ID)
		card := page.Contents[0]
		if card.Counts.Favorites != nil || card.Counts.Likes != nil || card.Version.Pages != nil || card.Excerpt != "" {
			t.Fatalf("locked card leaked restricted data: %+v", card)
		}
		control, err := admin.GetContent(t.Context(), locked.ID, ck.GetContentQuery{})
		must(t, err)
		if control.Body == "" {
			t.Fatal("body denial lacked a stored-body control")
		}
	})
	t.Run("CK-J07-S02_known_ID_hydration_keeps_visibility_and_order", func(t *testing.T) {
		page, err := public.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{two.ID, excluded.ID, draft.ID, future.ID, deleted.ID, locked.ID, one.ID}, Mode: "hide"})
		must(t, err)
		assertCards(t, page, one.ID, two.ID)
		if page.Contents[0].ID != two.ID || page.Contents[1].ID != one.ID {
			t.Fatal("known-ID hydration changed caller order")
		}
		visible, err := wide.GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{excluded.ID}})
		must(t, err)
		assertCards(t, visible, excluded.ID)
	})
	t.Run("CK-J07-S17_public_lists_leave_deleted_versions_out_even_for_staff", func(t *testing.T) {
		// Staff find deleted content in management, not among public cards.
		page, err := admin.GetContents(t.Context(), ck.GetContentsQuery{Ids: []string{deleted.ID, one.ID}})
		must(t, err)
		assertCards(t, page, one.ID)
		managed, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{deleted.ID}})
		must(t, err)
		if len(managed) != 1 || managed[0].ID != deleted.ID {
			t.Fatalf("management lost the deleted content: %+v", managed)
		}
	})
	t.Run("CK-J07-S03_search_and_typeahead_conceal_internal_names_and_invisible_content", func(t *testing.T) {
		found, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, found.ContentPage, one.ID, two.ID)
		typeahead, err := public.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, discovery.ContentPage{Contents: typeahead.Contents}, one.ID, two.ID)
		secret, err := public.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: "HiddenClassificationNeedle", EntityKinds: []string{"tag"}})
		must(t, err)
		if len(secret.Entities) != 0 || len(secret.Contents) != 0 {
			t.Fatalf("public typeahead disclosed internal classification: %+v", secret)
		}
		secretSearch, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "HiddenClassificationNeedle", Kind: "article"})
		must(t, err)
		if len(secretSearch.Contents) != 0 {
			t.Fatalf("search indexed an internal classification: %+v", secretSearch)
		}
		include := true
		managed, err := admin.GetManageSearchSuggestions(t.Context(), ck.GetManageSearchSuggestionsQuery{Q: "HiddenClassificationNeedle", IncludeEntities: &include, EntityKinds: []string{"tag"}})
		must(t, err)
		if len(managed.Entities) != 1 || managed.Entities[0].ID != internal.ID {
			t.Fatalf("internal name positive control absent: %+v", managed)
		}
		_, err = public.GetManageSearchSuggestions(t.Context(), ck.GetManageSearchSuggestionsQuery{Q: "HiddenClassificationNeedle", IncludeEntities: &include, EntityKinds: []string{"tag"}})
		requireError(t, err, 401, "unauthorized")
	})
	t.Run("CK-J07-S09_search_finds_works_by_entity_alias_and_offers_visible_matching_entities", func(t *testing.T) {
		found, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "MandatoryAliasNeedle", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, found.ContentPage, one.ID, two.ID)
		if len(found.Entities) != 1 || found.Entities[0].ID != required.ID {
			t.Fatalf("search did not offer the tag matching its alias: %+v", found.Entities)
		}
		later, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "MandatoryAliasNeedle", Kind: "article", Mode: "hide", Page: 2})
		must(t, err)
		if len(later.Entities) != 0 {
			t.Fatalf("search offered entities past the first page: %+v", later.Entities)
		}
		hidden, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "ElsewhereOnlyNeedle", Kind: "article", Mode: "hide"})
		must(t, err)
		if len(hidden.Entities) != 0 || len(hidden.Contents) != 0 {
			t.Fatalf("search offered a tag whose only work is outside the domain: %+v", hidden)
		}
		control, err := wide.GetSearch(t.Context(), ck.GetSearchQuery{Q: "ElsewhereOnlyNeedle", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, control.ContentPage, excluded.ID)
		if len(control.Entities) != 1 || control.Entities[0].ID != elsewhere.ID {
			t.Fatalf("wide domain did not offer the tag of its visible work: %+v", control.Entities)
		}
	})
	rawGet := func(t *testing.T, token, path string) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL+"/content"+path, nil)
		must(t, err)
		request.Host = "restricted.invalid"
		presentToken(t, w.auth, request, token)
		response, err := http.DefaultClient.Do(request)
		must(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		must(t, err)
		return response, body
	}
	refused := func(t *testing.T, path string) {
		t.Helper()
		response, body := rawGet(t, "", path)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"invalid_request"`) {
			t.Fatalf("%s answered %d, want 400 invalid_request: %s", path, response.StatusCode, body)
		}
	}
	t.Run("CK-J07-S27_typeahead_and_entity_covers_stay_inside_the_read_boundary", func(t *testing.T) {
		outside, err := public.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: "ElsewhereOnlyNeedle", EntityKinds: []string{"tag"}})
		must(t, err)
		if len(outside.Entities) != 0 {
			t.Fatalf("typeahead suggested a tag whose only work is outside the domain: %+v", outside.Entities)
		}
		control, err := wide.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: "ElsewhereOnlyNeedle", EntityKinds: []string{"tag"}})
		must(t, err)
		if len(control.Entities) != 1 || control.Entities[0].ID != elsewhere.ID {
			t.Fatalf("wide domain lost the tag of its visible work: %+v", control.Entities)
		}
		// Both works publish at the fixed clock's instant, so favorites decide
		// the automatic cover: the favored one is outside the restricted domain.
		_, fan := w.person(t, 0)
		_, err = reader("wide.invalid", fan).PostContentFavorite(t.Context(), excluded.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		cover := func(t *testing.T, reader *ck.Client, want string) {
			t.Helper()
			suggested, err := reader.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: "ChoiceNeedle", EntityKinds: []string{"tag"}})
			must(t, err)
			found, err := reader.GetSearch(t.Context(), ck.GetSearchQuery{Q: "ChoiceNeedle", Kind: "article"})
			must(t, err)
			for _, cards := range [][]entities.EntityCard{suggested.Entities, found.Entities} {
				if len(cards) != 1 || cards[0].ID != optional.ID || cards[0].Cover == nil || cards[0].Cover.ContentID != want {
					got := ""
					if len(cards) == 1 && cards[0].Cover != nil {
						got = cards[0].Cover.ContentID
					}
					t.Fatalf("optional tag's automatic cover is content %q, want %s: %+v", got, want, cards)
				}
			}
		}
		cover(t, public, two.ID)
		cover(t, wide, excluded.ID)
	})
	t.Run("CK-J07-S28_oversized_queries_are_refused_before_retrieval", func(t *testing.T) {
		for _, q := range []string{
			strings.Repeat("a", 257),
			// NFKD spells each as six letters: one 132-letter word.
			strings.Repeat("\u3316", 22),
			strings.TrimSpace(strings.Repeat("word ", 17)),
		} {
			for _, route := range []string{"/v1/search", "/v1/typeahead"} {
				refused(t, route+"?q="+url.QueryEscape(q))
			}
		}
		response, body := rawGet(t, "", "/v1/search?q="+url.QueryEscape(strings.TrimSpace(strings.Repeat("CriticalVis ", 16))))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("a query at the bounds was refused: %d %s", response.StatusCode, body)
		}
	})
	t.Run("CK-J07-S29_malformed_cursors_seeds_and_zones_are_refused", func(t *testing.T) {
		cursor := func(fields map[string]string) string {
			raw, err := json.Marshal(fields)
			must(t, err)
			return base64.RawURLEncoding.EncodeToString(raw)
		}
		for _, path := range []string{
			"/v1/contents?kind=article&sort=newest&cursor=" + cursor(map[string]string{"s": "newest", "v": "not a time", "id": one.ID}),
			"/v1/contents?kind=article&sort=most_favorited&cursor=" + cursor(map[string]string{"s": "most_favorited", "v": "1.5", "id": one.ID}),
			"/v1/contents?kind=article&sort=newest&cursor=" + cursor(map[string]string{"s": "newest", "v": "2026-01-01T00:00:00Z", "id": "not-a-content-id"}),
			"/v1/contents?kind=article&sort=random&cursor=" + cursor(map[string]string{"s": "random", "v": "0", "id": one.ID, "r": strings.Repeat("f", 64)}),
			"/v1/contents?kind=article&released_on=2026-11&tz=Local",
			"/v1/release-dates?kind=article&tz=Local",
		} {
			refused(t, path)
		}
		first, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "newest", Limit: 1, Mode: "hide"})
		must(t, err)
		if len(first.Contents) != 1 || first.NextCursor == "" {
			t.Fatalf("first page: %+v", first)
		}
		next, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "newest", Limit: 1, Mode: "hide", Cursor: first.NextCursor})
		must(t, err)
		if len(next.Contents) != 1 || next.Contents[0].ID == first.Contents[0].ID {
			t.Fatalf("a served cursor no longer continues its list: %+v then %+v", first, next)
		}
	})
	t.Run("CK-J07-S30_codes_of_unknown_kinds_resolve_nothing", func(t *testing.T) {
		_, err := w.pool.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{w.schema, "content_codes"}.Sanitize()+` (tenant_id, code, content_kind, content_id, slug, slugs)
 VALUES ($1, 'RETRED00Z', 'retired', $2, '', '{}')`, a.cfg.Scope.AppID.String(), uuid.NewString())
		must(t, err)
		for _, c := range []*ck.Client{public, admin} {
			_, err = c.GetCode(t.Context(), "RETRED00Z", ck.GetCodeQuery{})
			requireError(t, err, http.StatusNotFound, "not_found")
		}
		_, err = public.GetCode(t.Context(), one.Code, ck.GetCodeQuery{})
		must(t, err)
	})
	t.Run("CK-J07-S31_json_reads_are_private_unless_declared_and_location_independent", func(t *testing.T) {
		directive := func(t *testing.T, token, path string, status int, want string) {
			t.Helper()
			response, body := rawGet(t, token, path)
			if response.StatusCode != status || response.Header.Get("Cache-Control") != want {
				t.Fatalf("%s: %d %q, want %d %q: %s", path, response.StatusCode, response.Header.Get("Cache-Control"), status, want, body)
			}
		}
		_, token := w.person(t, 0)
		directive(t, "", "/v1/contents/"+one.ID, http.StatusOK, "private, no-store")
		directive(t, "", "/v1/entities?kind=tag", http.StatusOK, "private, no-store")
		directive(t, "", "/v1/nowhere", http.StatusNotFound, "private, no-store")
		directive(t, "", "/v1/kinds", http.StatusOK, "public, max-age=60")
		directive(t, "", "/v1/catalog/totals?kinds=article", http.StatusOK, "public, max-age=600")
		directive(t, "", "/v1/catalog/totals?kinds=unconfigured", http.StatusBadRequest, "private, no-store")
		directive(t, token, "/v1/kinds", http.StatusOK, "private, no-store")
		site, err := admin.PostModerationLocationRules(t.Context(), content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "198.51.100.0/24"}}, Target: "site", Domains: []string{"main"}, Enabled: true})
		must(t, err)
		defer func() { must(t, admin.DeleteModerationLocationRule(t.Context(), site.ID)) }()
		// The rule covers other visitors: any cached answer would reach them.
		directive(t, "", "/v1/kinds", http.StatusOK, "private, no-store")
		directive(t, "", "/v1/catalog/totals?kinds=article", http.StatusOK, "private, no-store")
	})
	t.Run("CK-J07-S04_sitemap_calendar_and_totals_apply_domain_scope", func(t *testing.T) {
		page, err := public.GetSitemapContents(t.Context(), ck.GetSitemapContentsQuery{Kind: "article"})
		must(t, err)
		got := []string{}
		for _, entry := range page.Entries {
			got = append(got, entry.ID)
		}
		sort.Strings(got)
		want := []string{one.ID, two.ID, locked.ID}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("domain sitemap IDs=%v want=%v", got, want)
		}
		entityMap, err := public.GetSitemapEntities(t.Context(), ck.GetSitemapEntitiesQuery{Kind: "tag"})
		must(t, err)
		if len(entityMap.Entries) != 2 {
			t.Fatalf("entity sitemap lost public controls: %+v", entityMap)
		}
		for _, entry := range entityMap.Entries {
			if entry.ID == string(internal.ID) {
				t.Fatal("sitemap exposed an internal entity")
			}
		}
		calendar, err := public.GetReleases(t.Context(), ck.GetReleasesQuery{Kind: "article", Mode: "hide", Days: 1})
		must(t, err)
		cards := []discovery.Card{}
		for _, day := range calendar.Days {
			cards = append(cards, day.Contents...)
		}
		assertCards(t, discovery.ContentPage{Contents: cards}, one.ID, two.ID)
		dates, err := public.GetReleaseDates(t.Context(), ck.GetReleaseDatesQuery{Kind: "article", Mode: "hide", By: "day"})
		must(t, err)
		if len(dates.Dates) != 1 || dates.Dates[0].Count != 2 {
			t.Fatalf("domain release date count wrong: %+v", dates)
		}
		totals, err := public.GetCatalogTotals(t.Context(), ck.GetCatalogTotalsQuery{Kinds: []string{"article"}})
		must(t, err)
		if totals.Kinds["article"].Public != 2 {
			t.Fatalf("domain public total=%+v", totals)
		}
		wideTotals, err := wide.GetCatalogTotals(t.Context(), ck.GetCatalogTotalsQuery{Kinds: []string{"article"}})
		must(t, err)
		if wideTotals.Kinds["article"].Public != 3 {
			t.Fatalf("wide-domain positive control=%+v", wideTotals)
		}
	})
	t.Run("CK-J07-S05_reader_filter_cannot_disable_hard_requirement", func(t *testing.T) {
		_, filterToken := w.person(t, 0)
		filterReader := reader("restricted.invalid", filterToken)
		filters, err := filterReader.GetContentFilters(t.Context(), ck.GetContentFiltersQuery{})
		must(t, err)
		if len(filters.Filters) != 1 || filters.Filters[0].Entity.ID != optional.ID {
			t.Fatalf("required/internal filters exposed as optional: %+v", filters)
		}
		show := true
		hide := false
		updated, err := filterReader.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(optional.ID), Visible: &hide}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		if len(updated.Filters) != 1 || updated.Filters[0].Visible {
			t.Fatalf("allowed reader preference did not persist: %+v", updated)
		}
		_, err = filterReader.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(required.ID), Visible: &show}}}, ck.PatchMeContentFiltersQuery{})
		requireError(t, err, 400, "invalid_request")
		page, err := filterReader.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, page, one.ID)
		selected, err := filterReader.GetContents(t.Context(), ck.GetContentsQuery{Entities: []string{string(optional.ID)}, Mode: "hide"})
		must(t, err)
		assertCards(t, selected, two.ID)
	})
	t.Run("CK-J07-S10_search_naming_a_hidden_entity_shows_its_contents", func(t *testing.T) {
		_, token := w.person(t, 0)
		filterReader := reader("restricted.invalid", token)
		hide := false
		_, err := filterReader.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(optional.ID), Visible: &hide}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		named, err := filterReader.GetSearch(t.Context(), ck.GetSearchQuery{Q: "ChoiceNeedle", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, named.ContentPage, two.ID)
		unnamed, err := filterReader.GetSearch(t.Context(), ck.GetSearchQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, unnamed.ContentPage, one.ID)
	})
	t.Run("CK-J07-S15_popular_list_follows_the_stored_ranking", func(t *testing.T) {
		listed, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		if len(listed.Contents) < 2 {
			t.Fatalf("fixture lists %d articles; want at least two", len(listed.Contents))
		}
		var ranked []string
		for i := len(listed.Contents) - 1; i >= 0; i-- {
			ranked = append(ranked, listed.Contents[i].ID)
		}
		popular := func() []string {
			t.Helper()
			page, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular:30d", Mode: "hide"})
			must(t, err)
			var ids []string
			for _, card := range page.Contents {
				ids = append(ids, card.ID)
			}
			return ids
		}
		if got := popular(); len(got) != 0 {
			t.Fatalf("popular list before its ranking is built lists %v; want an empty prepared ranking", got)
		}
		_, err = w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_popular_rankings (tenant_id,kind,period,domain,language,computed_at,content_ids)
 VALUES ($1,'article','30d','main','',now(),$2)
 ON CONFLICT (tenant_id,kind,period,domain,language) DO UPDATE SET computed_at=excluded.computed_at,content_ids=excluded.content_ids`, a.cfg.Scope.AppID.String(), ranked)
		must(t, err)
		if got := popular(); !reflect.DeepEqual(got, ranked) {
			t.Fatalf("popular list order %v; want the stored ranking %v", got, ranked)
		}
	})
	t.Run("CK-J07-S16_public_requests_cannot_create_popularity_work", func(t *testing.T) {
		rankings := w.schema + ".content_popular_rankings"
		var before int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+rankings).Scan(&before))
		for _, q := range []ck.GetContentsQuery{
			{Kind: "article", Sort: "popular:unknown"},
			{Kind: "article", Sort: "popular:7d", Language: "xx"},
			{Kind: "missing", Sort: "popular:7d"},
		} {
			_, err := public.GetContents(t.Context(), q)
			requireError(t, err, 400, "invalid_request")
		}
		_, err := wide.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular:7d"})
		requireError(t, err, 400, "invalid_request")
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL+"/content/v1/contents?kind=article&sort=popular&activity_from=2025-11-01&activity_until=2025-11-24", nil)
		must(t, err)
		response, err := (&http.Client{Transport: domainTransport{host: "restricted.invalid"}}).Do(request)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("arbitrary activity range accepted: HTTP %d", response.StatusCode)
		}
		var after int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+rankings).Scan(&after))
		if after != before {
			t.Fatalf("public requests created ranking rows: %d -> %d", before, after)
		}
	})
	t.Run("CK-J07-S12_search_tolerates_one_edit_per_word", func(t *testing.T) {
		for q, want := range map[string][]string{
			"CriticalVsi": {one.ID, two.ID},
			"allowed twp": {two.ID},
			"allowed tow": {two.ID},
			"CritclVis":   {},
		} {
			found, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: q, Kind: "article", Mode: "hide"})
			must(t, err)
			assertCards(t, found.ContentPage, want...)
		}
	})
	t.Run("CK-J07-S13_short_typeahead_completes_name_prefixes", func(t *testing.T) {
		for q, want := range map[string][]string{"cr": {one.ID, two.ID}, "al": {}, "all": {one.ID, two.ID}} {
			got, err := public.GetTypeahead(t.Context(), ck.GetTypeaheadQuery{Q: q, Kind: "article", Mode: "hide"})
			must(t, err)
			assertCards(t, discovery.ContentPage{Contents: got.Contents}, want...)
		}
	})
	t.Run("CK-J07-S06_code_resolution_retains_parent_and_domain_authority", func(t *testing.T) {
		resolved, err := public.GetCode(t.Context(), one.Code, ck.GetCodeQuery{V: one.Versions[0].Code})
		must(t, err)
		if resolved.ContentID != one.ID || resolved.VersionID != one.Versions[0].ID {
			t.Fatalf("visible code positive control mismatched: %+v", resolved)
		}
		for _, hidden := range []catalog.Content{excluded, draft, future} {
			_, err := public.GetCode(t.Context(), hidden.Code, ck.GetCodeQuery{})
			requireError(t, err, 404, "not_found")
		}
		wideLink, err := wide.GetCode(t.Context(), excluded.Code, ck.GetCodeQuery{})
		must(t, err)
		if wideLink.ContentID != excluded.ID {
			t.Fatal("domain refusal lacked a visible parent control")
		}
	})
	t.Run("CK-API-103_open_alias_lookup_preserves_app_and_visibility_boundaries", func(t *testing.T) {
		batch := importer.Batch{Source: "critical-aliases", Aliases: []importer.AliasRecord{
			{Kind: "legacy", Key: "visible", Locator: "page-3", Target: importer.TargetRef{Content: &importer.Ref{ID: one.ID}}},
			{Kind: "legacy", Key: "draft", Locator: "private-draft-position", Target: importer.TargetRef{Content: &importer.Ref{ID: draft.ID}}},
			{Kind: "legacy", Key: "internal", Locator: "private-entity-position", Target: importer.TargetRef{Entity: &importer.Ref{ID: string(internal.ID)}}},
		}}
		result, err := admin.PostImport(t.Context(), batch)
		must(t, err)
		if len(result.Failed) != 0 || result.Written["aliases"] != 3 {
			t.Fatalf("real alias import failed: %+v", result)
		}
		query := contenturl.AliasLookup{Aliases: []contenturl.AliasKey{{Source: batch.Source, Kind: "legacy", Key: "visible"}, {Source: batch.Source, Kind: "legacy", Key: "draft"}, {Source: batch.Source, Kind: "legacy", Key: "internal"}}}
		for _, client := range []*ck.Client{wide, reader("wide.invalid", "invalid-credential-is-ignored-on-open-route")} {
			resolved, err := client.PostCodesAliases(t.Context(), query, ck.PostCodesAliasesQuery{})
			must(t, err)
			if len(resolved.Aliases) != 3 || resolved.Aliases[0].Status != "found" || resolved.Aliases[0].Resolved == nil || resolved.Aliases[0].Resolved.ContentID != one.ID || resolved.Aliases[0].Locator != "page-3" {
				t.Fatalf("public alias positive control absent: %+v", resolved)
			}
			for _, hidden := range resolved.Aliases[1:] {
				if hidden.Status != "not_found" || hidden.Resolved != nil || hidden.Locator != "" {
					t.Fatalf("open alias exposed private target: %+v", hidden)
				}
			}
		}
		foreign, err := w.apps[1].client("").PostCodesAliases(t.Context(), query, ck.PostCodesAliasesQuery{})
		must(t, err)
		if len(foreign.Aliases) != 3 {
			t.Fatal("foreign alias result lost request order")
		}
		for _, hidden := range foreign.Aliases {
			if hidden.Status != "not_found" || hidden.Resolved != nil || hidden.Locator != "" {
				t.Fatal("alias lookup crossed app storage")
			}
		}
	})
	t.Run("CK-J07-S07_public_entity_views_omit_internal_identity_and_associations", func(t *testing.T) {
		_, err := public.GetEntity(t.Context(), string(internal.ID), ck.GetEntityQuery{})
		requireError(t, err, 404, "not_found")
		managed, err := admin.GetManageEntity(t.Context(), string(internal.ID), ck.GetManageEntityQuery{})
		must(t, err)
		if managed.Code == "" {
			t.Fatal("internal entity code positive fixture missing")
		}
		for name, client := range map[string]*ck.Client{"anonymous": public, "person": viewer, "host": admin} {
			t.Run(name+"_internal_code", func(t *testing.T) {
				leaked, err := client.GetCode(t.Context(), managed.Code, ck.GetCodeQuery{})
				if err == nil {
					wire, _ := json.Marshal(leaked)
					t.Fatalf("internal entity code disclosed public identity/path: %s", wire)
				}
				requireError(t, err, 404, "not_found")
			})
		}
		publicEntity, err := admin.GetManageEntity(t.Context(), string(required.ID), ck.GetManageEntityQuery{})
		must(t, err)
		for name, client := range map[string]*ck.Client{"anonymous": public, "person": viewer, "host": admin} {
			t.Run(name+"_public_code", func(t *testing.T) {
				resolved, err := client.GetCode(t.Context(), publicEntity.Code, ck.GetCodeQuery{})
				must(t, err)
				if resolved.ContentID != string(required.ID) {
					t.Fatalf("public entity code changed target: %+v", resolved)
				}
			})
		}
		_, ownerToken, owned := ownedChannel(t, w, a, "Code visibility owner")
		ownedDetail, err := a.client(ownerToken).GetChannel(t.Context(), string(owned.ID), ck.GetChannelQuery{})
		must(t, err)
		ownerLink, err := a.client(ownerToken).GetCode(t.Context(), ownedDetail.Code, ck.GetCodeQuery{})
		must(t, err)
		if ownerLink.ContentID != string(owned.ID) {
			t.Fatal("owned public entity code changed target")
		}
		encoded, err := json.Marshal(managed)
		must(t, err)
		if !strings.Contains(string(encoded), "HiddenClassificationNeedle") {
			t.Fatal("admin internal entity control missing")
		}
		entitiesPage, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag"})
		must(t, err)
		encoded, err = json.Marshal(entitiesPage)
		must(t, err)
		if strings.Contains(string(encoded), string(internal.ID)) || strings.Contains(string(encoded), "HiddenClassificationNeedle") {
			t.Fatal("public entity listing leaked internal metadata")
		}
		item, err := public.GetContent(t.Context(), one.ID, ck.GetContentQuery{})
		must(t, err)
		encoded, err = json.Marshal(item)
		must(t, err)
		if strings.Contains(string(encoded), string(internal.ID)) || strings.Contains(string(encoded), "HiddenClassificationNeedle") {
			t.Fatal("public content leaked internal association")
		}
	})
	t.Run("CK-J07-S08_pagination_similar_and_recommendations_keep_exclusions", func(t *testing.T) {
		cards := []discovery.Card{}
		cursor := ""
		for i := 0; i < 4; i++ {
			page, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Mode: "hide", Limit: 1, Cursor: cursor})
			must(t, err)
			cards = append(cards, page.Contents...)
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = page.NextCursor
		}
		assertCards(t, discovery.ContentPage{Contents: cards}, one.ID, two.ID)
		// Similar never lists content the reader's policy excludes.
		similar, err := public.GetContentSimilar(t.Context(), one.ID, ck.GetContentSimilarQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		for _, card := range similar.Contents {
			if card.ID != two.ID {
				t.Fatalf("similar listed excluded content %s", card.ID)
			}
		}
		recommended, err := viewer.GetRecommendations(t.Context(), ck.GetRecommendationsQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, recommended, one.ID, two.ID)
	})
	t.Run("location_rules_cover_all_visitor_read_paths", func(t *testing.T) {
		user, token := w.person(t, 0)
		signed := reader("restricted.invalid", token)
		for _, item := range []catalog.Content{one, two} {
			_, err := signed.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
			must(t, err)
			opened(t, signed, item.ID)
			postComment(t, signed, item.ID, "Location rule comment fixture")
		}
		waitForWithin(t, 30*time.Second, func() bool {
			return logged(t, a.deps.ClickHouse, a.cfg.ClickHouse.Database, a.cfg.Scope.AppID.String(), uuid.MustParse(user.ID)) >= 4
		})
		w.runAnalytics(t)
		before, err := signed.GetMeHistory(t.Context(), ck.GetMeHistoryQuery{Kind: "article"})
		must(t, err)
		assertCards(t, before, one.ID, two.ID)
		favorites, err := signed.GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{Kind: "article"})
		must(t, err)
		if len(favorites.Items) != 2 {
			t.Fatal("missing favorite positive controls")
		}
		create := func(in content.LocationRuleInput) content.LocationRule {
			t.Helper()
			rule, err := admin.PostModerationLocationRules(t.Context(), in)
			must(t, err)
			return rule
		}
		direct := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "127.0.0.0/8"}}, Target: "content", ContentIDs: []string{one.ID}, Domains: []string{"main"}, Enabled: true})
		tag := create(content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "127.0.0.0/8"}}, Target: "entities", EntityIDs: []string{string(optional.ID)}, Domains: []string{"main"}, Enabled: true})
		defer func() {
			must(t, admin.DeleteModerationLocationRule(t.Context(), direct.ID))
			must(t, admin.DeleteModerationLocationRule(t.Context(), tag.ID))
		}()
		listed, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, listed)
		found, err := public.GetSearch(t.Context(), ck.GetSearchQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, found.ContentPage)
		_, err = public.GetContentSimilar(t.Context(), one.ID, ck.GetContentSimilarQuery{Kind: "article", Mode: "hide"})
		requireError(t, err, 404, "not_found")
		recommended, err := signed.GetRecommendations(t.Context(), ck.GetRecommendationsQuery{Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, recommended)
		history, err := signed.GetMeHistory(t.Context(), ck.GetMeHistoryQuery{Kind: "article"})
		must(t, err)
		assertCards(t, history)
		favorites, err = signed.GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{Kind: "article"})
		must(t, err)
		if len(favorites.Items) != 0 {
			t.Fatal("favorites exposed covered content")
		}
		for _, item := range []catalog.Content{one, two} {
			_, err = public.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			requireError(t, err, 404, "not_found")
			_, err = signed.GetContentComments(t.Context(), item.ID, ck.GetContentCommentsQuery{})
			requireError(t, err, 404, "not_found")
			_, err = admin.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			requireError(t, err, 404, "not_found")
			_, err = public.GetCode(t.Context(), item.Code, ck.GetCodeQuery{})
			requireError(t, err, 404, "not_found")
			_, err = admin.GetCode(t.Context(), item.Code, ck.GetCodeQuery{})
			requireError(t, err, 404, "not_found")
			_, err = a.engine.VisitorClient(netip.MustParseAddr("203.0.113.45"), "restricted.invalid").GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			must(t, err)
		}
		uncovered, err := a.engine.VisitorClient(netip.MustParseAddr("203.0.113.45"), "restricted.invalid").GetSearch(t.Context(), ck.GetSearchQuery{Q: "CriticalVis", Kind: "article", Mode: "hide"})
		must(t, err)
		assertCards(t, uncovered.ContentPage, one.ID, two.ID)
	})

}

func TestExactVersionVisibility(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		article := cfg.Content.Kinds["article"]
		article.Versions.Hidden = false
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	item := publishedArticle(t, a, "Exact version selection")
	futureAt := w.clock.now().Add(time.Hour)
	locked, err := admin.PostContentVersions(t.Context(), item.ID, catalog.VersionInput{Languages: []string{"de"}, State: catalog.StatePublished, Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}})
	must(t, err)
	awaitEditionPublication(t, a, admin, item.ID, locked.ID)
	future, err := admin.PostContentVersions(t.Context(), item.ID, catalog.VersionInput{Languages: []string{"en"}, State: catalog.StatePublished, PublishAt: &futureAt})
	must(t, err)
	t.Run("CK-J07-S06_locked_version_ID_and_code_never_select_open_sibling", func(t *testing.T) {
		got, err := reader.GetVersion(t.Context(), locked.ID, ck.GetVersionQuery{})
		must(t, err)
		if got.SelectedVersionID != locked.ID || got.Decision.Allowed || got.Body != "" || len(got.Versions) != 1 || got.Versions[0].ID != locked.ID {
			t.Fatalf("locked edition fell back: %+v", got)
		}
		resolved, err := reader.GetCode(t.Context(), item.Code, ck.GetCodeQuery{V: locked.Code})
		must(t, err)
		if resolved.VersionID != locked.ID {
			t.Fatalf("locked code selected sibling: %+v", resolved)
		}
		_, err = reader.GetVersion(t.Context(), future.ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		// The current code resolver returns a public parent link and an
		// unresolved VersionID. It does not expose the hidden edition or
		// claim that an accessible sibling matched the explicit code.
		unresolved, err := reader.GetCode(t.Context(), item.Code, ck.GetCodeQuery{V: future.Code})
		must(t, err)
		if unresolved.VersionID != "" || strings.Contains(unresolved.Path, future.Code) {
			t.Fatalf("future code disclosed a hidden edition: %+v", unresolved)
		}
		control, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !control.Decision.Allowed || control.Body == "" {
			t.Fatal("open sibling positive control absent")
		}
	})
}

func configureDiscoverySignals(t *testing.T, w *world, a *application) {
	t.Helper()
	addr, user, password := os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR"), os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")
	if addr == "" || user == "" || password == "" {
		t.Fatal("owned ClickHouse required for selected recommendation behavior")
	}
	database := w.schema + "_signals"
	a.cfg.ClickHouse = ck.ClickHouseConfig{Addr: addr, Username: user, Password: password, Database: database, Cluster: os.Getenv("CONTENTKIT_CRITICAL_CH_CLUSTER")}
	must(t, ck.MigrateClickHouse(t.Context(), w.pool, a.cfg))
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: database, Username: user, Password: password}, DialTimeout: time.Second})
	must(t, err)
	a.deps.ClickHouse = conn
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a.server.Close()
		must(t, a.engine.Close(ctx))
		must(t, conn.Exec(ctx, "DROP DATABASE IF EXISTS "+database+" SYNC"))
		must(t, conn.Close())
	})
}
