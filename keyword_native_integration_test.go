package contentkit

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
)

// profileDB creates a disposable database from CONTENTKIT_PROFILE_URL (a
// server with PGroonga and, for the legacy lineage, pgvector) and applies the
// lineage into schema "app".
func profileDB(t *testing.T, ctx context.Context, prefix string, lineage fs.FS) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_PROFILE_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_PROFILE_URL not set (PGroonga + vector server)")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	db := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{db}.Sanitize()) })
	cfg := admin.Config()
	cfg.ConnConfig.Database = db
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if lineage != nil {
		applyLineage(t, ctx, pool, lineage, nil)
	}
	return pool
}

// applyLineage applies every file of a lineage into schema app, recording the
// ledger, and calls before(name) ahead of each file.
func applyLineage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lineage fs.FS, before func(name string)) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS app; CREATE TABLE IF NOT EXISTS public.applied_migrations(name text PRIMARY KEY,checksum text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := fs.ReadDir(lineage, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		if before != nil {
			before(entry.Name())
		}
		sql, err := fs.ReadFile(lineage, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SET LOCAL search_path TO app,public"); err == nil {
			_, err = tx.Exec(ctx, string(sql))
		}
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO public.applied_migrations VALUES($1,$2)", entry.Name(), fmt.Sprintf("%x", []byte(entry.Name())))
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeywordNativeIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := profileDB(t, ctx, "keyword_native", migrations.Postgres)
	const schema = "app"
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema, Tenant: testTenant})
	if err != nil {
		t.Fatal(err)
	}
	// Ids ascend in the fixtures' former lexical order.
	accent, accentMany, alias, canonical, hyphen := cid(1), cid(2), cid(3), cid(4), cid(5)
	keyword, native, negative, unvoiced, voiced := cid(6), cid(7), cid(8), cid(9), cid(10)
	documents := map[string]map[string]KeywordDocument{
		"en": {canonical: doc("gallery", canonical, "en", "Not Guilty", nil, nil), alias: doc("gallery", alias, "en", "Court Drama", []string{"Not Guilty"}, nil), keyword: doc("gallery", keyword, "en", "Trial Story", nil, []string{"Not Guilty"}), negative: doc("gallery", negative, "en", "Guilty", nil, nil), accent: doc("gallery", accent, "en", "Café Moon", nil, nil), hyphen: doc("gallery", hyphen, "en", "Two-Factor", nil, nil), accentMany: doc("gallery", accentMany, "en", "Résumé façonné", nil, nil)},
		"ja": {native: doc("gallery", native, "ja", "鬼滅の刃", []string{"Demon Slayer"}, nil), negative: doc("gallery", negative, "ja", "魔法少女", nil, nil), voiced: doc("gallery", voiced, "ja", "ガンダム", nil, nil), unvoiced: doc("gallery", unvoiced, "ja", "カンタム", nil, nil)},
		"zh": {native: doc("gallery", native, "zh", "魔法少女", nil, nil), negative: doc("gallery", negative, "zh", "鬼滅の刃", nil, nil)},
		"ko": {native: doc("gallery", native, "ko", "마법소녀", nil, nil), negative: doc("gallery", negative, "ko", "바다소년", nil, nil)},
	}
	var all []KeywordDocument
	for _, docs := range documents {
		for _, d := range docs {
			all = append(all, d)
		}
	}
	upsertDocs(t, ctx, pool, schema, all...)
	for _, tc := range []struct{ lang, query, id string }{
		{"en", "Not Guilty", canonical}, {"en", "not giulty", canonical}, {"en", "not guily", canonical}, {"en", "not guillty", canonical}, {"en", "not guxlty", canonical},
		{"en", "ＮＯＴ　ＧＵＩＬＴＹ", canonical}, {"en", "not gui", canonical}, {"en", "ca", accent}, {"en", "Café Moon", accent}, {"en", "Two-Factor", hyphen}, {"en", "cafe moon", accent}, {"en", "resume faconne", accentMany},
		{"ja", "鬼滅の刃", native}, {"ja", "鬼滅の刀", native}, {"ja", "鬼の刃", native}, {"ja", "鬼滅なの刃", native}, {"ja", "鬼の滅刃", native}, {"ja", "滅鬼の刃", native}, {"ja", "の刃", native}, {"ja", "demon slayer", native}, {"ja", "ｶﾞﾝﾀﾞﾑ", voiced}, {"ja", "ガンダム", voiced}, {"ja", "カンタム", unvoiced},
		{"zh", "魔法少如", native}, {"zh", "魔少女", native}, {"zh", "魔法小少女", native}, {"zh", "魔法女少", native}, {"zh", "法魔少女", native},
		{"ko", "마법소너", native}, {"ko", "마소녀", native}, {"ko", "마법작소녀", native}, {"ko", "마법녀소", native}, {"ko", "법마소녀", native}, {"ko", "마법소녀", native},
	} {
		t.Run(tc.lang+"/"+tc.query, func(t *testing.T) {
			page, trace, err := client.SearchWithTrace(ctx, tc.query, SearchOptions{Language: tc.lang, ContentKinds: []string{"gallery"}, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			hits := page.Hits
			if len(hits) == 0 || hits[0].ContentID != tc.id || hits[0].Version() != "" || hits[0].TenantID != testTenant || hits[0].Language != tc.lang || page.HasMore || page.Truncated {
				t.Fatalf("hits=%v want first %s", hits, tc.id)
			}
			if trace.Sources[0].Backend != BackendKeyword {
				t.Fatalf("trace=%+v", trace)
			}
			suggestions, err := client.Typeahead(ctx, tc.query, TypeaheadOptions{Language: tc.lang, ContentKinds: []string{"gallery"}, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(suggestions) == 0 || suggestions[0].ContentID != tc.id || suggestions[0].Score <= 0 {
				t.Fatalf("suggestions=%v", suggestions)
			}
		})
	}
	for _, tc := range []struct{ lang, query string }{{"en", "not aquarium"}, {"en", "not guxxxy"}, {"ja", "鬼刀"}, {"ja", "鬼月の刀"}, {"zh", "魔刀"}, {"zh", "海洋世界"}, {"ko", "마너"}, {"ko", "바다세계"}, {"en", "not OR guilty"}} {
		page, err := client.Search(ctx, tc.query, SearchOptions{Language: tc.lang, ContentKinds: []string{"gallery"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 0 || page.HasMore {
			t.Errorf("negative %s/%s page=%+v", tc.lang, tc.query, page)
		}
	}
	// Server and client ceilings apply even without a host-provided deadline.
	t.Run("budgets", func(t *testing.T) {
		page, err := client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, FilterSQL: "current_setting('statement_timeout')='2s'"})
		if err != nil || len(page.Hits) != 3 {
			t.Fatalf("statement budget: %v %v", page, err)
		}
		for _, duration := range []time.Duration{25 * time.Millisecond, 10 * time.Second} {
			request, cancel := context.WithTimeout(ctx, duration)
			started := time.Now()
			_, err := client.Search(request, "Not Guilty", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, FilterSQL: "EXISTS (SELECT 1 FROM pg_sleep(5))"})
			cancel()
			if err == nil {
				t.Fatal("unbounded query completed")
			}
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Fatalf("request exceeded keyword ceiling: %s", elapsed)
			}
		}
	})
	page, err := client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	hits := page.Hits
	if len(hits) != 3 || hits[0].ContentID != canonical || hits[1].ContentID != alias || hits[2].ContentID != keyword || page.HasMore {
		t.Fatalf("tier order=%v", page)
	}
	if hits[0].Score != 1 || hits[1].Score != .9 || hits[2].Score != .75 {
		t.Fatalf("lexical scores are match tiers: %+v", hits)
	}
	page, err = client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}, FilterSQL: "sd.content_id = @eligible", FilterArgs: map[string]any{"eligible": alias}})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ContentID != alias {
		t.Fatalf("host filter page=%v err=%v", page, err)
	}
	// An update replaces old aliases/keywords atomically; an empty title deletes.
	upsertDocs(t, ctx, pool, schema, doc("gallery", alias, "en", "Court Drama", nil, nil), doc("gallery", canonical, "en", "", nil, nil))
	page, err = client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", ContentKinds: []string{"gallery"}})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ContentID != keyword {
		t.Fatalf("update/delete page=%v err=%v", page, err)
	}
	// Real filler catalog. EXPLAIN must prove an indexed fuzzy scan without forcing
	// enable_seqscan off; a query that only works on the tiny fixture is insufficient.
	if _, err = pool.Exec(ctx, `INSERT INTO app.content_search_documents(tenant_id,content_kind,content_id,content_version_id,language,title,raw_document) SELECT 'doujins','gallery','01920000-0000-7000-9000-'||lpad(i::text,12,'0'),'','zh','占位内容'||md5(i::text),'filler' FROM generate_series(1,50000) i; ANALYZE app.content_search_documents`); err != nil {
		t.Fatal(err)
	}
	var trgm string
	if err = pool.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE extname='pg_trgm'`).Scan(&trgm); err != nil {
		t.Fatal(err)
	}
	qt := pgx.Identifier{trgm}.Sanitize()
	if _, err = pool.Exec(ctx, `SET pg_trgm.word_similarity_threshold=0.1`); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE,BUFFERS,COSTS OFF) SELECT sd.content_id FROM app.content_search_documents sd WHERE sd.tenant_id='doujins' AND sd.language='zh' AND app.contentkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.%%>) '魔法女少' ORDER BY app.contentkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.<->>) '魔法女少' LIMIT 100`, qt, qt))
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	t.Log(plan.String())
	if !strings.Contains(plan.String(), "content_search_documents_keyword_fuzzy") || strings.Contains(plan.String(), "Seq Scan") {
		t.Fatalf("expected indexed bounded fuzzy path:\n%s", plan.String())
	}
	page, err = client.Search(ctx, "魔法女少", SearchOptions{Language: "zh", ContentKinds: []string{"gallery"}})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ContentID != native || page.HasMore {
		t.Fatalf("large catalog page=%v err=%v", page, err)
	}
	_ = search.MaxCandidateLimit
}
