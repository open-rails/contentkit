package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

func TestVersionOwnedContent(t *testing.T) {
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	w := newWorld(t, func(c *ck.Config) {
		c.Content = kinds.Config{Languages: []string{"en", "zh"},
			EntityKinds: map[string]kinds.EntityKind{"tag": {Route: "tags", MembershipScopes: []kinds.MembershipScope{kinds.ContentScope, kinds.VersionScope}}},
			Kinds:       map[string]kinds.Kind{"post": {Route: "posts", Presentation: "post", Entities: []string{"tag"}, Versions: kinds.VersionRules{Languages: "one"}}, "blog": {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}}}
	})
	a := w.apps[0]
	pool, schema, cfg := w.pool, w.schema, a.cfg
	engine, server := a.engine, a.server
	admin, public := engine.Client(), a.client("")
	must(engine.StartJobs(ctx))
	diagnose := func(id string) {
		t.Helper()
		for _, table := range []string{"content_versions", "content_publication_requests", "content_search_documents", "content_search_dirty", "content_search_invalid"} {
			var rows string
			err := pool.QueryRow(ctx, "SELECT coalesce(jsonb_agg(to_jsonb(t)),'[]'::jsonb)::text FROM "+pgx.Identifier{schema, table}.Sanitize()+" t WHERE content_id=$1", id).Scan(&rows)
			t.Logf("%s: %s (%v)", table, rows, err)
		}
	}
	create := func(kind, language, title, body string) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(ctx, catalog.ContentInput{Kind: kind, Versions: []catalog.VersionInput{{Languages: []string{language}, Text: map[string]catalog.VersionText{language: {Title: title, Description: "Summary " + language, Body: body}}}}})
		must(err)
		return item
	}
	publish := func(item catalog.Content) catalog.Content {
		t.Helper()
		_, err := admin.PostContentPublish(ctx, item.ID, catalog.PublishInput{})
		must(err)
		until := time.Now().Add(20 * time.Second)
		for time.Now().Before(until) {
			// Publication is per edition; an already-live default edition does
			// not prove that a newly authored sibling has finished releasing.
			got, err := admin.GetContent(ctx, item.ID, ck.GetContentQuery{})
			must(err)
			ready := len(got.Versions) > 0
			for _, version := range got.Versions {
				ready = ready && version.Live
			}
			if ready {
				return got
			}
			time.Sleep(30 * time.Millisecond)
		}
		diagnose(item.ID)
		t.Fatal("version publication did not complete")
		return catalog.Content{}
	}
	item := create("post", "en", "English title", "English contentkit:i-11111111-1111-4111-8111-111111111111")
	en := item.Versions[0].ID
	var zh string
	t.Run("initial_and_alternative_versions_own_their_text", func(t *testing.T) {
		if item.Title != "English title" || item.Body != "English contentkit:i-11111111-1111-4111-8111-111111111111" {
			t.Fatalf("wrong initial projection: %+v", item)
		}
		added, err := admin.PostContentVersions(ctx, item.ID, catalog.VersionInput{Languages: []string{"zh"}})
		must(err)
		zh = added.ID
		if len(added.Text) != 0 || added.Title != "" || added.Body != "" || added.Cover != nil && (added.Cover.Image != "" || len(added.Cover.Renditions) > 0) {
			t.Fatalf("new version borrowed content: %+v", added)
		}

		if zh == "" {
			t.Fatal("new version absent")
		}
		_, err = admin.PatchContentVersion(ctx, item.ID, zh, catalog.VersionInput{Text: map[string]catalog.VersionText{"zh": {Title: "中文标题", Description: "中文简介", Body: "中文 contentkit:i-22222222-2222-4222-8222-222222222222"}}, State: catalog.StatePublished})
		must(err)
		got, err := admin.GetVersion(ctx, en, ck.GetVersionQuery{})
		must(err)
		if got.Title != "English title" || strings.Contains(got.Body, "22222222") {
			t.Fatal("Chinese edit changed English version")
		}
		got, err = admin.GetVersion(ctx, zh, ck.GetVersionQuery{})
		must(err)
		if got.Title != "中文标题" || strings.Contains(got.Body, "11111111") {
			t.Fatal("Chinese version borrowed English content")
		}
	})
	t.Run("feeds_select_text_and_summary_from_the_same_edition", func(t *testing.T) {
		publish(item)
		for _, test := range []struct{ lang, title, summary string }{{"en", "English title", "Summary en"}, {"zh", "中文标题", "中文简介"}} {
			page, err := public.GetContents(ctx, ck.GetContentsQuery{Kind: "post", Language: test.lang, DisplayLanguage: test.lang})
			must(err)
			if len(page.Contents) != 1 || page.Contents[0].Title != test.title || page.Contents[0].Excerpt != test.summary {
				diagnose(item.ID)
				t.Fatalf("wrong language card: %+v", page)
			}
		}
	})
	t.Run("title_sort_and_cursor_use_the_displayed_edition", func(t *testing.T) {
		makePair := func(enTitle, zhTitle string) catalog.Content {
			pair, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "post", Versions: []catalog.VersionInput{
				{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: enTitle, Body: "English body"}}},
				{Languages: []string{"zh"}, Text: map[string]catalog.VersionText{"zh": {Title: zhTitle, Body: "Chinese body"}}},
			}})
			must(err)
			publish(pair)
			return pair
		}
		zulu, mango := makePair("Zulu English", "Aardvark Chinese"), makePair("Mango English", "Zebra Chinese")
		query := ck.GetContentsQuery{Kind: "post", Ids: []string{zulu.ID, mango.ID}, Language: "en", DisplayLanguage: "zh", Sort: "title", Limit: 1}
		first, err := public.GetContents(ctx, query)
		must(err)
		if len(first.Contents) != 1 || first.Contents[0].ID != mango.ID || first.Contents[0].Title != "Mango English" || first.Contents[0].Version.ID != mango.Versions[0].ID || first.NextCursor == "" {
			t.Fatalf("sort borrowed sibling text: %+v", first)
		}
		query.Cursor = first.NextCursor
		second, err := public.GetContents(ctx, query)
		must(err)
		if len(second.Contents) != 1 || second.Contents[0].ID != zulu.ID || second.Contents[0].Title != "Zulu English" || second.Contents[0].Version.ID != zulu.Versions[0].ID {
			t.Fatalf("title cursor changed selected edition: %+v", second)
		}
	})
	t.Run("search_and_typeahead_keep_the_matching_version", func(t *testing.T) {
		searchable, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "post", Versions: []catalog.VersionInput{
			{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Ordinary default", Aliases: []string{"TwinOrchid"}, Body: "Default only"}}},
			{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Crimson Orchid", Aliases: []string{"NacrePearl", "TwinOrchid"}, Description: "Matched summary", Body: "Matched body"}}},
			{Languages: []string{"zh"}, Text: map[string]catalog.VersionText{"zh": {Title: "Chinese MoonLanguage", Body: "中文专有"}}},
			{Languages: []string{"en"}, Access: &access.Rule{Level: access.PPV}, Text: map[string]catalog.VersionText{"en": {Title: "SecretCabinet", Body: "Protected body"}}},
		}})
		must(err)
		publish(searchable)
		// The existing search worker coalesces immediate updates and retains
		// a 30-second backstop for a write that races a running sync.
		deadline := time.Now().Add(45 * time.Second)
		indexed := false
		for time.Now().Before(deadline) {
			var n int
			must(pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "content_search_documents"}.Sanitize()+" WHERE tenant_id=$1 AND content_id=$2 AND content_version_id IS NOT NULL", cfg.Scope.AppID.String(), searchable.ID).Scan(&n))
			if n == 4 {
				indexed = true
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		if !indexed {
			diagnose(searchable.ID)
			t.Fatal("version documents did not become searchable")
		}
		includeSummary := true
		twins, err := public.GetSearch(ctx, ck.GetSearchQuery{Q: "TwinOrchid", Kind: "post", Language: "en", Mode: "hide", Summary: &includeSummary, Limit: 1})
		must(err)
		if len(twins.Contents) != 1 || twins.Contents[0].ID != searchable.ID || twins.NextCursor != "" || twins.Summary == nil || twins.Summary.Total != 1 {
			t.Fatalf("two matching editions duplicated one work or pagination: %+v", twins)
		}
		for _, sort := range []string{"relevance", "newest", "title"} {
			page, err := public.GetSearch(ctx, ck.GetSearchQuery{Q: "NacrePearl", Kind: "post", Language: "en", Mode: "hide", Sort: sort, Summary: &includeSummary})
			must(err)
			if len(page.Contents) != 1 || page.Contents[0].Version.ID != searchable.Versions[1].ID || page.Contents[0].Title != "Crimson Orchid" || page.Contents[0].Excerpt != "Matched summary" || page.Summary == nil || page.Summary.Total != 1 {
				t.Fatalf("%s search hydrated another version: %+v", sort, page)
			}
			link, err := url.Parse(page.Contents[0].Path)
			must(err)
			if link.Query().Get("v") != searchable.Versions[1].Code {
				t.Fatalf("%s search link opened a different version: %s", sort, page.Contents[0].Path)
			}
			private, err := public.GetSearch(ctx, ck.GetSearchQuery{Q: "SecretCabinet", Kind: "post", Language: "en", Mode: "hide", Sort: sort, Summary: &includeSummary})
			must(err)
			if len(private.Contents) != 0 || private.Summary == nil || private.Summary.Total != 0 {
				t.Fatalf("%s search unlocked a private sibling through a free version: %+v", sort, private)
			}
		}
		suggestion, err := public.GetTypeahead(ctx, ck.GetTypeaheadQuery{Q: "NacrePearl", Kind: "post", Language: "en", Mode: "hide"})
		must(err)
		if len(suggestion.Contents) != 1 || suggestion.Contents[0].Version.ID != searchable.Versions[1].ID {
			t.Fatalf("typeahead borrowed default edition: %+v", suggestion)
		}
		wrongLanguage, err := public.GetTypeahead(ctx, ck.GetTypeaheadQuery{Q: "MoonLanguage", Kind: "post", Language: "en", Mode: "hide"})
		must(err)
		if len(wrongLanguage.Contents) != 0 {
			t.Fatalf("typeahead matched a different-language edition: %+v", wrongLanguage)
		}
		locked, err := public.GetTypeahead(ctx, ck.GetTypeaheadQuery{Q: "SecretCabinet", Kind: "post", Language: "en", Mode: "hide"})
		must(err)
		if len(locked.Contents) != 0 {
			t.Fatal("typeahead exposed a locked matching sibling")
		}
		tags, err := admin.PostEntities(ctx, []entities.EntityInput{{Kind: "tag", Slug: "edition-keyword", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "EditionMetadataNeedle"}}}})
		must(err)
		_, err = admin.PutContentVersionEntities(ctx, searchable.ID, searchable.Versions[1].ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tags[0].ID}}}, ck.PutContentVersionEntitiesQuery{})
		must(err)
		waitKeyword := func(word string) {
			t.Helper()
			until := time.Now().Add(45 * time.Second)
			for time.Now().Before(until) {
				page, err := public.GetSearch(ctx, ck.GetSearchQuery{Q: word, Kind: "post", Language: "en", Mode: "hide"})
				must(err)
				if len(page.Contents) == 1 && page.Contents[0].Version.ID == searchable.Versions[1].ID {
					return
				}
				time.Sleep(40 * time.Millisecond)
			}
			diagnose(searchable.ID)
			t.Fatal("edition-specific metadata did not select its own version: " + word)
		}
		waitKeyword("EditionMetadataNeedle")
		_, err = admin.PatchEntity(ctx, string(tags[0].ID), entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "RenamedEditionNeedle"}}})
		must(err)
		waitKeyword("RenamedEditionNeedle")
	})
	t.Run("parent_text_writes_are_not_an_alias", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/content/v1/contents/"+item.ID+"/text/en", strings.NewReader(`{"title":"wrong parent"}`))
		must(err)
		res, err := server.Client().Do(req)
		must(err)
		res.Body.Close()
		if res.StatusCode != 404 && res.StatusCode != 405 {
			t.Fatalf("retired text route status %d", res.StatusCode)
		}
		var raw map[string]any
		b, err := json.Marshal(item)
		must(err)
		must(json.Unmarshal(b, &raw))
		if _, ok := raw["text"]; ok {
			t.Fatal("parent still returns writable localized text map")
		}
		var count int
		must(pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='content_text'", schema).Scan(&count))
		if count != 0 {
			t.Fatal("parent text storage remains")
		}
	})
	t.Run("hidden_initial_version_and_import_use_the_same_contract", func(t *testing.T) {
		blog := create("blog", "zh", "Hidden Chinese", "Body of hidden edition")
		publish(blog)
		origin := "原題"
		batch := importer.Batch{Source: "version-owned-test", Contents: []importer.ContentRecord{{ExternalID: "post", ID: item.ID, Kind: "post", State: catalog.StatePublished, Versions: []importer.VersionRecord{{ExternalID: "zh", ID: zh, Languages: []string{"zh"}, OriginalTitle: &origin, Text: map[string]catalog.VersionText{"zh": {Title: "导入标题", Body: "只属于中文版"}}}}}}}
		result, err := admin.PostImport(ctx, batch)
		must(err)
		if len(result.Failed) != 0 {
			t.Fatalf("version import: %+v", result)
		}
		got, err := admin.GetVersion(ctx, zh, ck.GetVersionQuery{})
		must(err)
		if got.Title != "导入标题" || got.Body != "只属于中文版" || got.OriginalTitle != origin {
			t.Fatalf("wrong imported version text: %+v", got)
		}
		got, err = admin.GetVersion(ctx, en, ck.GetVersionQuery{})
		must(err)
		if got.Title != "English title" {
			t.Fatal("import changed sibling")
		}
	})
}
