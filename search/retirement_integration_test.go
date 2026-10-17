package search_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

func TestRetireLegacySearchIndexes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	qs := pgx.Identifier{schema}.Sanitize()
	// The operator must work even with just one available pool connection.
	cfg := pool.Config()
	cfg.MaxConns = 1
	operator, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(operator.Close)
	const tenant = "retirement"
	first, second, entity := contentref.NewID(), contentref.NewID(), contentref.NewID()
	docs := []search.KeywordDocument{
		{DocumentKey: search.DocumentKey{ContentRef: contentref.New(tenant, "gallery", first), Language: "en"}, Title: "The Strawberry Chronicle", Aliases: []string{"The Berry Book"}, Keywords: []string{"Alice"}},
		{DocumentKey: search.DocumentKey{ContentRef: contentref.New(tenant, "gallery", second), Language: "en"}, Title: "Blueberry Tales"},
	}
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, docs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+qs+`.content_groups(tenant_id,id,kind,slug) VALUES($1,$2,'tag','the-cafe')`, tenant, entity); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+qs+`.content_group_names(tenant_id,group_id,language,kind,name)
 VALUES($1,$2,'en','name','The Café'),($1,$2,'en','alias','Café')`, tenant, entity); err != nil {
		t.Fatal(err)
	}
	options := search.Options{Schema: schema, Tenant: tenant, Language: "en", ContentKinds: []string{"gallery"}, Limit: 10}
	read := func() []search.Result {
		t.Helper()
		var out []search.Result
		for _, q := range []struct {
			find func(context.Context, *pgxpool.Pool, string, search.Options) (search.Result, error)
			text string
		}{{search.NameSearch, "berry"}, {search.NameSearch, "Alice"}, {search.KeywordSearch, "The Alice"}, {search.KeywordSearch, "strawberry"}} {
			result, err := q.find(ctx, pool, q.text, options)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, result)
		}
		return out
	}
	before := read()
	if len(before[0].Hits) != 2 || len(before[1].Hits) != 0 || len(before[2].Hits) != 1 || before[2].Hits[0].ContentID != first {
		t.Fatalf("native search fixture: %+v", before)
	}
	old := []string{"content_search_documents_keyword_exact", "content_search_documents_keyword_fuzzy", "content_search_documents_keyword_native", "content_search_documents_name_prefix"}
	oldCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=ANY($2::text[])`, schema, old).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := search.RetireLegacyIndexes(ctx, operator, schema, false); err == nil || oldCount() != 4 {
		t.Fatalf("retired without the explicit cohort flag: %v", err)
	}
	// Missing replacement coverage fails before deleting any legacy index.
	if _, err := pool.Exec(ctx, "DROP INDEX "+qs+".content_search_documents_names_substring"); err != nil {
		t.Fatal(err)
	}
	if _, err := search.RetireLegacyIndexes(ctx, operator, schema, true); err == nil || !strings.Contains(err.Error(), "replacement index") || oldCount() != 4 {
		t.Fatalf("missing replacement was accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX content_search_documents_names_substring ON `+qs+`.content_search_documents
 USING pgroonga (`+qs+`.contentkit_search_names(title,aliases) public.pgroonga_text_regexp_ops_v2)`); err != nil {
		t.Fatal(err)
	}
	// Simulate a prior interrupted pass. SQL-standard function dependencies
	// must still be found when one old index's regclass is already NULL.
	if _, err := pool.Exec(ctx, "DROP INDEX CONCURRENTLY "+qs+"."+old[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION `+qs+`.unexpected_consumer(title text) RETURNS text LANGUAGE sql IMMUTABLE
 RETURN `+qs+`.contentkit_keyword_text(title,'{}'::text[],'{}'::text[],'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := search.RetireLegacyIndexes(ctx, operator, schema, true); err == nil || !strings.Contains(err.Error(), "unexpected SQL dependency") || oldCount() != 3 {
		t.Fatalf("tracked helper dependency was ignored: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP FUNCTION "+qs+".unexpected_consumer(text)"); err != nil {
		t.Fatal(err)
	}
	// Finish an interrupted pass, then prove an idempotent retry; each reports what it removed.
	finished, err := search.RetireLegacyIndexes(ctx, operator, schema, true)
	if err != nil {
		t.Fatal(err)
	}
	wrappers := []string{"contentkit_keyword_terms(text,text[],text[],text)", "contentkit_keyword_text(text,text[],text[],text)"}
	if len(finished.Validated) != 10 || !reflect.DeepEqual(finished.Dropped, append(slices.Clone(old[1:]), wrappers...)) || !reflect.DeepEqual(finished.Absent, old[:1]) {
		t.Fatalf("interrupted pass report: %+v", finished)
	}
	retry, err := search.RetireLegacyIndexes(ctx, operator, schema, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Validated) != 10 || len(retry.Dropped) != 0 || !reflect.DeepEqual(retry.Absent, append(slices.Clone(old), wrappers...)) {
		t.Fatalf("idempotent retry report: %+v", retry)
	}
	if oldCount() != 0 {
		t.Fatal("legacy indexes remain")
	}
	for _, name := range []string{"contentkit_keyword_terms", "contentkit_keyword_text"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regprocedure($1) IS NOT NULL`, qs+"."+name+"(text,text[],text[],text)").Scan(&exists); err != nil || exists {
			t.Fatalf("retired helper %s still exists: %v", name, err)
		}
	}
	if after := read(); !reflect.DeepEqual(after, before) {
		t.Fatalf("retirement changed native name/full search: before=%+v after=%+v", before, after)
	}
	// Both SQL dependency edges which must survive: the generated identity
	// column and the replacement search normalizer still use keyword_normalize.
	var identity, matched string
	var columnEdge, functionEdge bool
	if err := pool.QueryRow(ctx, `SELECT `+qs+`.contentkit_keyword_normalize('The Café'),`+qs+`.contentkit_search_normalize('The Café'),
 EXISTS(SELECT 1 FROM pg_depend WHERE refclassid='pg_proc'::regclass AND refobjid=to_regprocedure($1) AND classid='pg_attrdef'::regclass),
 EXISTS(SELECT 1 FROM pg_depend WHERE refclassid='pg_proc'::regclass AND refobjid=to_regprocedure($1) AND classid='pg_proc'::regclass AND objid=to_regprocedure($2))`,
		qs+".contentkit_keyword_normalize(text)", qs+".contentkit_search_normalize(text)").Scan(&identity, &matched, &columnEdge, &functionEdge); err != nil || identity != "the cafe" || matched != "cafe" || !columnEdge || !functionEdge {
		t.Fatalf("identity/search dependency graph changed: %q %q column=%v function=%v %v", identity, matched, columnEdge, functionEdge, err)
	}
	_, duplicate := pool.Exec(ctx, `INSERT INTO `+qs+`.content_group_names(tenant_id,group_id,language,kind,name) VALUES($1,$2,'en','alias','THE CAFE')`, tenant, entity)
	var pgErr *pgconn.PgError
	if !errors.As(duplicate, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("retirement changed canonical alias uniqueness: %v", duplicate)
	}
	var names int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.content_group_names WHERE tenant_id=$1 AND group_id=$2`, tenant, entity).Scan(&names); err != nil || names != 2 {
		t.Fatalf("identity's filler distinction was lost: %d %v", names, err)
	}
	managed := options
	managed.ContentKinds = []string{"tag"}
	managed.Eligibility = &search.Eligibility{SQL: "SELECT 0 AS priority"}
	if result, err := search.NameSearchFrom(ctx, pool, "The Cafe", managed, search.CurrentEntityNames); err != nil || len(result.Hits) != 1 || result.Hits[0].ContentID != entity {
		t.Fatalf("current management names broke after retirement: %+v %v", result, err)
	}
	docs[0].Title = "The Strawberry Revised"
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, docs[:1]); err != nil {
		t.Fatalf("current writer broke after retirement: %v", err)
	}
	if result, err := search.NameSearch(ctx, pool, "revised", options); err != nil || len(result.Hits) != 1 || result.Hits[0].ContentID != first {
		t.Fatalf("new indexed names were not searchable: %+v %v", result, err)
	}
}
