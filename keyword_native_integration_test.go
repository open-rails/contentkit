package searchkit

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
	"github.com/open-rails/searchkit/migrations"
	"github.com/open-rails/searchkit/pg"
)

// installKeywordFields upgrades old minimal fixtures just as an existing host
// applies the additive migration. The applied baseline is deliberately untouched.
func installKeywordFields(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	sql, err := fs.ReadFile(migrations.Postgres, "0003_keyword_fields.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgroonga`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SET LOCAL search_path TO `+pgx.Identifier{schema}.Sanitize()+`,public`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestKeywordNativeIntegration(t *testing.T) {
	dsn := os.Getenv("SEARCHKIT_PROFILE_URL")
	if dsn == "" {
		t.Skip("SEARCHKIT_PROFILE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db := fmt.Sprintf("keyword_native_db_%d", time.Now().UnixNano())
	if _, err = pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin := pool
	defer admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{db}.Sanitize())
	cfg := admin.Config()
	cfg.ConnConfig.Database = db
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("keyword_native_%d", time.Now().UnixNano())
	qs := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(ctx, `CREATE SCHEMA `+qs); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), `DROP SCHEMA `+qs+` CASCADE`)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL search_path TO `+qs+`,public`); err != nil {
		t.Fatal(err)
	}
	files, err := fs.ReadDir(migrations.KeywordPostgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		sql, err := fs.ReadFile(migrations.KeywordPostgres, f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f.Name(), err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string]map[string]pg.KeywordDocument{
		"en": {"canonical": {Title: "Not Guilty"}, "alias": {Title: "Court Drama", Aliases: []string{"Not Guilty"}}, "keyword": {Title: "Trial Story", Keywords: []string{"Not Guilty"}}, "negative": {Title: "Guilty"}, "accent": {Title: "Café Moon"}, "hyphen": {Title: "Two-Factor"}, "accent-many": {Title: "Résumé façonné"}},
		"ja": {"native": {Title: "鬼滅の刃", Aliases: []string{"Demon Slayer"}}, "negative": {Title: "魔法少女"}, "voiced": {Title: "ガンダム"}, "unvoiced": {Title: "カンタム"}},
		"zh": {"native": {Title: "魔法少女"}, "negative": {Title: "鬼滅の刃"}},
		"ko": {"native": {Title: "마법소녀"}, "negative": {Title: "바다소년"}},
	}
	for lang, docs := range documents {
		if err = pg.UpsertKeywordDocuments(ctx, pool, schema, "gallery", lang, docs); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ lang, query, id string }{
		{"en", "Not Guilty", "canonical"}, {"en", "not giulty", "canonical"}, {"en", "not guily", "canonical"}, {"en", "not guillty", "canonical"}, {"en", "not guxlty", "canonical"},
		{"en", "ＮＯＴ　ＧＵＩＬＴＹ", "canonical"}, {"en", "not gui", "canonical"}, {"en", "ca", "accent"}, {"en", "Cafe\u0301 Moon", "accent"}, {"en", "Two-Factor", "hyphen"}, {"en", "cafe moon", "accent"}, {"en", "resume faconne", "accent-many"},
		{"ja", "鬼滅の刃", "native"}, {"ja", "鬼滅の刀", "native"}, {"ja", "鬼の刃", "native"}, {"ja", "鬼滅なの刃", "native"}, {"ja", "鬼の滅刃", "native"}, {"ja", "滅鬼の刃", "native"}, {"ja", "の刃", "native"}, {"ja", "demon slayer", "native"}, {"ja", "ｶﾞﾝﾀﾞﾑ", "voiced"}, {"ja", "ガンダム", "voiced"}, {"ja", "カンタム", "unvoiced"},
		{"zh", "魔法少如", "native"}, {"zh", "魔少女", "native"}, {"zh", "魔法小少女", "native"}, {"zh", "魔法女少", "native"}, {"zh", "法魔少女", "native"},
		{"ko", "마법소너", "native"}, {"ko", "마소녀", "native"}, {"ko", "마법작소녀", "native"}, {"ko", "마법녀소", "native"}, {"ko", "법마소녀", "native"}, {"ko", "마법소녀", "native"},
	} {
		t.Run(tc.lang+"/"+tc.query, func(t *testing.T) {
			hits, trace, err := client.SearchWithTrace(ctx, tc.query, SearchOptions{Language: tc.lang, EntityTypes: []string{"gallery"}, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) == 0 || hits[0].EntityID != tc.id {
				t.Fatalf("hits=%v want first %s", hits, tc.id)
			}
			if trace.Sources[0].Backend != BackendKeyword {
				t.Fatalf("trace=%+v", trace)
			}
			suggestions, err := client.Typeahead(ctx, tc.query, TypeaheadOptions{Language: tc.lang, EntityTypes: []string{"gallery"}, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(suggestions) == 0 || suggestions[0].EntityID != tc.id || suggestions[0].Score <= 0 {
				t.Fatalf("suggestions=%v", suggestions)
			}
		})
	}
	for _, tc := range []struct{ lang, query string }{{"en", "not aquarium"}, {"en", "not guxxxy"}, {"ja", "鬼刀"}, {"ja", "鬼月の刀"}, {"zh", "魔刀"}, {"zh", "海洋世界"}, {"ko", "마너"}, {"ko", "바다세계"}, {"en", "not OR guilty"}} {
		hits, err := client.Search(ctx, tc.query, SearchOptions{Language: tc.lang, EntityTypes: []string{"gallery"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("negative %s/%s hits=%v", tc.lang, tc.query, hits)
		}
	}
	// Server and client ceilings apply even without a host-provided deadline.
	t.Run("budgets", func(t *testing.T) {
		hits, err := client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}, FilterSQL: "current_setting('statement_timeout')='2s'"})
		if err != nil || len(hits) != 3 {
			t.Fatalf("statement budget: %v %v", hits, err)
		}
		for _, duration := range []time.Duration{25 * time.Millisecond, 10 * time.Second} {
			request, cancel := context.WithTimeout(ctx, duration)
			started := time.Now()
			_, err := client.Search(request, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}, FilterSQL: "EXISTS (SELECT 1 FROM pg_sleep(5))"})
			cancel()
			if err == nil {
				t.Fatal("unbounded query completed")
			}
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Fatalf("request exceeded keyword ceiling: %s", elapsed)
			}
		}
	})
	hits, err := client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || hits[0].EntityID != "canonical" || hits[1].EntityID != "alias" || hits[2].EntityID != "keyword" {
		t.Fatalf("tier order=%v", hits)
	}
	hits, err = client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}, FilterSQL: "sd.entity_id = @eligible", FilterArgs: map[string]any{"eligible": "alias"}})
	if err != nil || len(hits) != 1 || hits[0].EntityID != "alias" {
		t.Fatalf("host filter hits=%v err=%v", hits, err)
	}
	if err = pg.UpsertKeywordDocuments(ctx, pool, schema, "gallery", "en", map[string]pg.KeywordDocument{"alias": {Title: "Court Drama"}, "canonical": {}}); err != nil {
		t.Fatal(err)
	}
	hits, err = client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}})
	if err != nil || len(hits) != 1 || hits[0].EntityID != "keyword" {
		t.Fatalf("update/delete hits=%v err=%v", hits, err)
	}
	// The legacy public writer must replace old typed fields as well: no stale
	// aliases/keywords survive a subsequent string-only update.
	if err = pg.UpsertSearchDocuments(ctx, pool, schema, "gallery", "en", map[string]string{"keyword": "Sea breeze"}); err != nil {
		t.Fatal(err)
	}
	hits, err = client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: []string{"gallery"}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("legacy update retained fields: %v %v", hits, err)
	}
	// Real filler catalog. EXPLAIN must prove an indexed fuzzy scan without forcing
	// enable_seqscan off; a query that only works on the tiny fixture is insufficient.
	if _, err = pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.search_documents(entity_type,entity_id,language,title,document,raw_document) SELECT 'gallery','filler-'||i,'zh','占位内容'||md5(i::text),'filler','filler' FROM generate_series(1,50000) i; ANALYZE %s.search_documents`, qs, qs)); err != nil {
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
	rows, err := pool.Query(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE,BUFFERS,COSTS OFF) SELECT sd.entity_id FROM %s.search_documents sd WHERE sd.language='zh' AND %s.searchkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.%%>) '魔法女少' ORDER BY %s.searchkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.<->>) '魔法女少' LIMIT 100`, qs, qs, qt, qs, qt))
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
	if !strings.Contains(plan.String(), "search_documents_keyword_fuzzy") || strings.Contains(plan.String(), "Seq Scan") {
		t.Fatalf("expected indexed bounded fuzzy path:\n%s", plan.String())
	}
	hits, err = client.Search(ctx, "魔法女少", SearchOptions{Language: "zh", EntityTypes: []string{"gallery"}})
	if err != nil || len(hits) != 1 || hits[0].EntityID != "native" {
		t.Fatalf("large catalog hits=%v err=%v", hits, err)
	}
}
