package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

// This public-contract owner needs no other scenario's fixture, so the deliberate
// version API hard cut can be qualified while older callers are being adapted.
func TestVersionOwnedContent(t *testing.T) {
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	dsn := os.Getenv("CONTENTKIT_CRITICAL_PG")
	if dsn == "" {
		t.Fatal("CONTENTKIT_CRITICAL_PG is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	must(err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS pgroonga")
	must(err)
	schema := "ck259_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	defer func() {
		for _, suffix := range []string{"_river", ""} {
			_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema + suffix}.Sanitize()+" CASCADE")
			if err != nil {
				t.Error(err)
			}
		}
	}()
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool }))
	cfg := ck.Config{Tenant: "version-owned", Database: ck.DatabaseConfig{Schema: schema}, Content: kinds.Config{Languages: []string{"en", "zh"},
		Kinds: map[string]kinds.Kind{"post": {Route: "posts", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}, "blog": {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}}}}
	engine, err := ck.New(ctx, cfg, ck.Deps{Postgres: pool, Auth: ak.Authenticator()})
	must(err)
	defer func() { must(engine.Close(context.Background())) }()
	routes, err := engine.Routes(ck.Routes{Prefix: "/content", Groups: engine.RouteGroups()})
	must(err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Path, r.Handler)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	admin, public := engine.Client(), ck.NewClient(server.URL+"/content", nil)
	must(engine.Start(ctx))
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
			got, err := public.GetContent(ctx, item.ID, ck.GetContentQuery{})
			if err == nil && got.Live {
				return got
			}
			time.Sleep(30 * time.Millisecond)
		}
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
				t.Fatalf("wrong language card: %+v", page)
			}
		}
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

// Initialize using a published binary lineage, then prove the forward migration
// preserves existing identities and authored text without any source-media work.
func TestVersionContentMigration(t *testing.T) {
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	dsn := os.Getenv("CONTENTKIT_CRITICAL_PG")
	if dsn == "" {
		t.Fatal("CONTENTKIT_CRITICAL_PG is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	check(err)
	t.Cleanup(pool.Close)
	schema := "ck259_old_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:18]
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, suffix := range []string{"_river", ""} {
			_, err := pool.Exec(clean, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema + suffix}.Sanitize()+" CASCADE")
			if err != nil {
				t.Error(err)
			}
		}
	}()
	command := exec.CommandContext(ctx, "go", "run", "-p=1", ".")
	command.Dir = "previous-release/version-content"
	command.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=1", "CONTENTKIT_PREVIOUS_TEXT_SCHEMA="+schema)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("published initializer: %v\n%s", err, stderr.String())
	}
	var ids map[string]string
	check(json.Unmarshal(output, &ids))
	if ids["version"] != "v0.129.0" || ids["en"] == "" || ids["zh"] == "" {
		t.Fatalf("wrong published fixture: %+v", ids)
	}
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	snapshot := func(query string, args ...any) string {
		t.Helper()
		var raw string
		check(pool.QueryRow(ctx, query, args...).Scan(&raw))
		return raw
	}
	ledgerSQL := `SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY sequence),'[]'::jsonb)::text FROM public.migrations m WHERE app='contentkit' AND schema=$1`
	beforeLedger := snapshot(ledgerSQL, schema)
	versionsSQL := "SELECT jsonb_agg(to_jsonb(v)-'original_title'-'original_language' ORDER BY id)::text FROM " + table("content_versions") + " v WHERE tenant_id='version-owned-migration'"
	parentsSQL := "SELECT jsonb_agg(to_jsonb(c)-'original_title'-'original_language' ORDER BY id)::text FROM " + table("content_items") + " c WHERE tenant_id='version-owned-migration'"
	beforeVersions, beforeParents := snapshot(versionsSQL), snapshot(parentsSQL)
	cfg := ck.Config{Tenant: "version-owned-migration", Database: ck.DatabaseConfig{Schema: schema}, Content: kinds.Config{Languages: []string{"en", "zh"}, Kinds: map[string]kinds.Kind{"post": {Route: "posts", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}}
	check(ck.MigratePostgres(ctx, pool, cfg))
	t.Run("retained_translations_and_version_overrides_survive", func(t *testing.T) {
		for _, version := range []string{ids["en"], ids["zh"]} {
			for _, lang := range []string{"en", "zh", "ja"} {
				var title, slug, summary, body, sort, original, originalLanguage string
				var aliases []string
				check(pool.QueryRow(ctx, `SELECT t.title,t.slug,t.description,t.body,t.sort_key,t.aliases,v.original_title,v.original_language FROM `+table("content_version_text")+` t JOIN `+table("content_versions")+` v ON v.tenant_id=t.tenant_id AND v.id=t.version_id WHERE t.tenant_id='version-owned-migration' AND t.version_id=$1 AND t.language=$2`, version, lang).Scan(&title, &slug, &summary, &body, &sort, &aliases, &original, &originalLanguage))
				wantTitle, wantSummary, wantBody, wantSlug, wantSort := "Parent "+lang, "Summary "+lang, "Body "+lang, "parent-"+lang, "Order "+lang
				wantAliases := []string{"Alias " + lang}
				if lang == "ja" {
					wantTitle, wantSummary, wantBody, wantSlug, wantSort = "原題", "元の概要", "元の本文", "original", "original-order"
					wantAliases = []string{"別名"}
				}
				if version == ids["en"] && lang == "en" {
					wantTitle, wantSummary = "Edition override", "Edition summary"
				}
				if title != wantTitle || slug != wantSlug || summary != wantSummary || body != wantBody || sort != wantSort || !reflect.DeepEqual(aliases, wantAliases) || original != "原題" || originalLanguage != "ja" {
					t.Fatalf("lost authored data for %s/%s: title=%q slug=%q summary=%q body=%q sort=%q aliases=%v original=%q/%s", version, lang, title, slug, summary, body, sort, aliases, original, originalLanguage)
				}
			}
		}
		if snapshot(versionsSQL) != beforeVersions || snapshot(parentsSQL) != beforeParents {
			t.Fatal("text migration changed identity, access, revision or publication fields")
		}
		var parentTable *string
		check(pool.QueryRow(ctx, "SELECT to_regclass($1)::text", schema+".content_text").Scan(&parentTable))
		if parentTable != nil {
			t.Fatal("writable parent text table remains")
		}
	})
	t.Run("replay_preserves_adopted_text_and_published_migration_receipts", func(t *testing.T) {
		textSQL := "SELECT jsonb_agg(to_jsonb(t) ORDER BY version_id,language)::text FROM " + table("content_version_text") + " t"
		beforeText := snapshot(textSQL)
		check(ck.MigratePostgres(ctx, pool, cfg))
		if snapshot(textSQL) != beforeText {
			t.Fatal("migration replay changed adopted version text")
		}
		var oldRows, newRows []map[string]any
		check(json.Unmarshal([]byte(beforeLedger), &oldRows))
		check(json.Unmarshal([]byte(snapshot(ledgerSQL, schema)), &newRows))
		bySequence := map[any]map[string]any{}
		for _, row := range newRows {
			bySequence[row["sequence"]] = row
		}
		for _, row := range oldRows {
			if !reflect.DeepEqual(row, bySequence[row["sequence"]]) {
				t.Fatal("published migration receipt changed")
			}
		}
	})
}
