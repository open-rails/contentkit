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

// The host model behind these fixtures: item -> version, each version owning a
// language, publication state, a default flag and traits. Effective keywords
// are work tags plus that version's traits; siblings never contribute.
const groupedEligibilitySQL = `
SELECT v.item_id AS parent_id, (NOT v.is_default)::int AS priority
FROM host.versions v JOIN host.items i ON i.id = v.item_id
WHERE v.id = sd.entity_id AND v.language = sd.language AND v.live AND NOT i.deleted
  AND v.traits @> @traits::text[]`

func groupedEligibility(traits ...string) *Eligibility {
	if traits == nil {
		traits = []string{}
	}
	return &Eligibility{SQL: groupedEligibilitySQL, Args: map[string]any{"traits": traits}}
}

type groupedFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
}

type version struct {
	id, item, lang string
	live, def      bool
	traits         []string
	doc            pg.KeywordDocument
}

func (f groupedFixture) add(versions ...version) {
	f.t.Helper()
	for _, v := range versions {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO host.items(id) VALUES($1) ON CONFLICT DO NOTHING`, v.item); err != nil {
			f.t.Fatal(err)
		}
		traits := v.traits
		if traits == nil {
			traits = []string{}
		}
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO host.versions(id,item_id,language,live,is_default,traits) VALUES($1,$2,$3,$4,$5,$6)`, v.id, v.item, v.lang, v.live, v.def, traits); err != nil {
			f.t.Fatal(err)
		}
		if err := pg.UpsertKeywordDocuments(f.ctx, f.pool, f.schema, "item_version", v.lang, map[string]pg.KeywordDocument{v.id: v.doc}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f groupedFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func TestKeywordGroupedEligibilityIntegration(t *testing.T) {
	dsn := os.Getenv("SEARCHKIT_PROFILE_URL")
	if dsn == "" {
		t.Skip("SEARCHKIT_PROFILE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db := fmt.Sprintf("keyword_grouped_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{db}.Sanitize())
	cfg := admin.Config()
	cfg.ConnConfig.Database = db
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const schema = "sk"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE SCHEMA sk; SET LOCAL search_path TO sk,public`); err != nil {
		t.Fatal(err)
	}
	files, err := fs.ReadDir(migrations.KeywordPostgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		sql, err := fs.ReadFile(migrations.KeywordPostgres, file.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", file.Name(), err)
		}
	}
	if _, err = tx.Exec(ctx, `CREATE SCHEMA host;
CREATE TABLE host.items(id text PRIMARY KEY, deleted boolean NOT NULL DEFAULT false);
CREATE TABLE host.versions(id text PRIMARY KEY, item_id text NOT NULL REFERENCES host.items(id), language text NOT NULL,
  live boolean NOT NULL DEFAULT true, is_default boolean NOT NULL DEFAULT false, traits text[] NOT NULL DEFAULT '{}')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f := groupedFixture{t, ctx, pool, schema}
	client, err := NewClient(ClientConfig{Pool: pool, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	types := []string{"item_version"}
	search := func(query, lang string, mode LanguageMode, opts SearchOptions) SearchResult {
		t.Helper()
		opts.Language, opts.LanguageMode, opts.EntityTypes = lang, mode, types
		if opts.Eligibility == nil {
			opts.Eligibility = groupedEligibility()
		}
		page, err := client.Search(ctx, query, opts)
		if err != nil {
			t.Fatalf("%s/%s: %v", lang, query, err)
		}
		return page
	}
	expect := func(page SearchResult, want ...string) {
		t.Helper()
		var got []string
		for _, h := range page.Hits {
			got = append(got, h.ParentID+"="+h.EntityID+"/"+h.Language)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("hits=%v want %v", got, want)
		}
	}

	colored := []string{"colored"}
	f.add(
		// One work translated three times; the English original is the default
		// but sorts after its colored sibling.
		version{"g1-en-a", "g1", "en", true, false, colored, pg.KeywordDocument{Title: "Not Guilty", Keywords: []string{"Drama", "Colored"}}},
		version{"g1-en-b", "g1", "en", true, true, nil, pg.KeywordDocument{Title: "Not Guilty", Keywords: []string{"Drama"}}},
		version{"g1-es-1", "g1", "es", true, true, nil, pg.KeywordDocument{Title: "No Culpable", Aliases: []string{"Not Guilty"}, Keywords: []string{"Drama"}}},
		version{"g1-es-2", "g1", "es", true, false, colored, pg.KeywordDocument{Title: "No Culpable", Aliases: []string{"Not Guilty"}, Keywords: []string{"Drama", "Colored"}}},
		version{"g1-ja-1", "g1", "ja", true, true, nil, pg.KeywordDocument{Title: "無罪", Aliases: []string{"Not Guilty"}}},
		// English Colored plus Spanish Original: no Spanish Colored exists.
		version{"g2-en-col", "g2", "en", true, true, colored, pg.KeywordDocument{Title: "Spring Story", Keywords: []string{"Colored"}}},
		version{"g2-es-orig", "g2", "es", true, true, nil, pg.KeywordDocument{Title: "Historia de Primavera", Aliases: []string{"Spring Story"}}},
		// A real Spanish Colored edition.
		version{"g3-es-col", "g3", "es", true, true, colored, pg.KeywordDocument{Title: "Verano", Aliases: []string{"Summer Story"}, Keywords: []string{"Colored"}}},
		// A draft colored sibling that must stay invisible.
		version{"g4-en-1", "g4", "en", true, true, nil, pg.KeywordDocument{Title: "Autumn Story"}},
		version{"g4-en-2", "g4", "en", false, false, colored, pg.KeywordDocument{Title: "Autumn Story Secret", Keywords: []string{"Colored"}}},
		// A deleted work with a live version.
		version{"g5-en-1", "g5", "en", true, true, nil, pg.KeywordDocument{Title: "Winter Story"}},
		// Native-script versions of one work.
		version{"g6-ja-1", "g6", "ja", true, true, nil, pg.KeywordDocument{Title: "鬼滅の刃"}},
		version{"g6-ja-2", "g6", "ja", true, false, colored, pg.KeywordDocument{Title: "鬼滅の刃", Keywords: []string{"カラー"}}},
		version{"g6-zh-1", "g6", "zh", true, true, nil, pg.KeywordDocument{Title: "鬼灭之刃"}},
		version{"g6-zh-2", "g6", "zh", true, false, nil, pg.KeywordDocument{Title: "鬼灭之刃"}},
		version{"g6-ko-1", "g6", "ko", true, true, nil, pg.KeywordDocument{Title: "귀멸의 칼날"}},
		version{"g6-ko-2", "g6", "ko", true, false, nil, pg.KeywordDocument{Title: "귀멸의 칼날"}},
	)
	f.exec(`UPDATE host.items SET deleted=true WHERE id='g5'`)

	t.Run("one card per item", func(t *testing.T) {
		page := search("Not Guilty", "en", LanguageModeExact, SearchOptions{})
		expect(page, "g1=g1-en-b/en")
		if page.Hits[0].Score != 1 || page.HasMore || page.Truncated {
			t.Fatalf("page=%+v", page)
		}
		expect(search("Not Guilty", "es", LanguageModeExact, SearchOptions{}), "g1=g1-es-1/es")
		expect(search("Not Guilty", "ja", LanguageModeExact, SearchOptions{}), "g1=g1-ja-1/ja")
		// Fallback returns the item once, represented by the requested language,
		// ranked by its best match in any searched language.
		page = search("Not Guilty", "es", LanguageModeFallbackEnglish, SearchOptions{})
		expect(page, "g1=g1-es-1/es")
		if page.Hits[0].Score != 1 {
			t.Fatalf("fallback rank score=%v", page.Hits[0].Score)
		}
		suggestions, err := client.Typeahead(ctx, "not gui", TypeaheadOptions{Language: "es", LanguageMode: LanguageModeFallbackEnglish, EntityTypes: types, Eligibility: groupedEligibility()})
		if err != nil || len(suggestions) != 1 || suggestions[0].ParentID != "g1" || suggestions[0].EntityID != "g1-es-1" || suggestions[0].Language != "es" {
			t.Fatalf("typeahead=%+v err=%v", suggestions, err)
		}
		// Native-script typo/prefix matches also collapse to one card.
		for _, tc := range []struct{ lang, query, want string }{
			{"ja", "鬼滅の刀", "g6=g6-ja-1/ja"}, {"zh", "鬼灭刃", "g6=g6-zh-1/zh"}, {"ko", "귀멸의 칼", "g6=g6-ko-1/ko"},
		} {
			expect(search(tc.query, tc.lang, LanguageModeExact, SearchOptions{}), tc.want)
		}
	})

	t.Run("spanish colored is not english colored plus spanish original", func(t *testing.T) {
		expect(search("colored", "en", LanguageModeExact, SearchOptions{}), "g1=g1-en-a/en", "g2=g2-en-col/en")
		expect(search("colored", "es", LanguageModeExact, SearchOptions{}), "g1=g1-es-2/es", "g3=g3-es-col/es")
		expect(search("Not Guilty", "es", LanguageModeExact, SearchOptions{Eligibility: groupedEligibility("colored")}), "g1=g1-es-2/es")
		expect(search("Spring Story", "es", LanguageModeExact, SearchOptions{}), "g2=g2-es-orig/es")
		expect(search("Spring Story", "es", LanguageModeExact, SearchOptions{Eligibility: groupedEligibility("colored")}))
		expect(search("Summer Story", "es", LanguageModeExact, SearchOptions{Eligibility: groupedEligibility("colored")}), "g3=g3-es-col/es")
		// Explicit fallback may surface the English colored edition, labelled as such.
		expect(search("Spring Story", "es", LanguageModeFallbackEnglish, SearchOptions{Eligibility: groupedEligibility("colored")}), "g2=g2-en-col/en")
		expect(search("Not Guilty", "es", LanguageModeFallbackEnglish, SearchOptions{Eligibility: groupedEligibility("colored")}), "g1=g1-es-2/es")
	})

	t.Run("invisible siblings never influence eligibility", func(t *testing.T) {
		expect(search("Autumn", "en", LanguageModeExact, SearchOptions{}), "g4=g4-en-1/en")
		expect(search("Secret", "en", LanguageModeExact, SearchOptions{}))
		expect(search("Autumn Story Secret", "en", LanguageModeExact, SearchOptions{}))
		expect(search("Autumn", "en", LanguageModeExact, SearchOptions{Eligibility: groupedEligibility("colored")}))
		expect(search("Winter", "en", LanguageModeExact, SearchOptions{}))
		f.exec(`UPDATE host.versions SET live=true WHERE id='g4-en-2'`)
		f.exec(`UPDATE host.items SET deleted=false WHERE id='g5'`)
		expect(search("Autumn Story Secret", "en", LanguageModeExact, SearchOptions{}), "g4=g4-en-2/en")
		expect(search("Autumn", "en", LanguageModeExact, SearchOptions{Eligibility: groupedEligibility("colored")}), "g4=g4-en-2/en")
		expect(search("Winter", "en", LanguageModeExact, SearchOptions{}), "g5=g5-en-1/en")
		f.exec(`UPDATE host.versions SET live=false WHERE id='g4-en-2'`)
		f.exec(`UPDATE host.items SET deleted=true WHERE id='g5'`)
	})

	t.Run("pages and has_more over grouped retrieval", func(t *testing.T) {
		// Twelve items with three interleaved English versions each; the
		// default version of every item sorts last by id.
		for v := 1; v <= 3; v++ {
			for i := 1; i <= 12; i++ {
				item := fmt.Sprintf("p%02d", i)
				f.add(version{fmt.Sprintf("pv%d-%s", v, item), item, "en", true, v == 3, nil, pg.KeywordDocument{Title: "Blue Ocean"}})
			}
		}
		var seen []string
		for offset := 0; ; offset += 5 {
			page := search("blue ocean", "en", LanguageModeExact, SearchOptions{Limit: 5, Offset: offset})
			if page.Truncated {
				t.Fatalf("offset %d truncated: %+v", offset, page)
			}
			for _, h := range page.Hits {
				if h.EntityID != "pv3-"+h.ParentID {
					t.Fatalf("representative must be the default version: %+v", h)
				}
				seen = append(seen, h.ParentID)
			}
			if page.HasMore != (offset+5 < 12) || len(page.Hits) != min(5, 12-offset) {
				t.Fatalf("offset %d page=%+v", offset, page)
			}
			if !page.HasMore {
				break
			}
		}
		if want := "p01 p02 p03 p04 p05 p06 p07 p08 p09 p10 p11 p12"; strings.Join(seen, " ") != want {
			t.Fatalf("pages=%v", seen)
		}
		// A window smaller than the matching documents is reported, keeps one
		// card per item, and stays consistent across pages at the same limit.
		first := search("blue ocean", "en", LanguageModeExact, SearchOptions{Limit: 5, CandidateLimit: 12})
		if !first.Truncated || !first.HasMore || len(first.Hits) != 5 {
			t.Fatalf("truncated page=%+v", first)
		}
		second := search("blue ocean", "en", LanguageModeExact, SearchOptions{Limit: 5, Offset: 5, CandidateLimit: 12})
		if len(second.Hits) != 5 || !second.HasMore {
			t.Fatalf("second truncated page=%+v", second)
		}
		for _, h := range append(first.Hits, second.Hits...) {
			if strings.HasPrefix(h.EntityID, "pv3-") {
				t.Fatalf("default version beyond the window cannot be chosen: %+v", h)
			}
		}
		beyond := search("blue ocean", "en", LanguageModeExact, SearchOptions{Limit: 5, Offset: 12, CandidateLimit: 12})
		if len(beyond.Hits) != 0 || !beyond.HasMore || !beyond.Truncated {
			t.Fatalf("page beyond window=%+v", beyond)
		}
		// Traced pages report the same items and their best document positions.
		traced, trace, err := client.SearchWithTrace(ctx, "blue ocean", SearchOptions{Language: "en", EntityTypes: types, Limit: 5, Offset: 5, Eligibility: groupedEligibility()})
		if err != nil || len(trace.Results) != 5 || trace.Results[0].Rank != 6 || trace.Results[0].Key.ParentID != "p06" || trace.Results[0].ScoreKind != ScoreKeywordMatch || trace.Results[0].Contributions[0].SourceRank != 6 {
			t.Fatalf("trace=%+v err=%v", trace, err)
		}
		if len(traced.Hits) != 5 || traced.Hits[0].ParentID != "p06" || trace.Sources[0].Candidates[0].Key.ParentID != "p01" {
			t.Fatalf("traced=%+v", traced)
		}
		for _, opts := range []SearchOptions{{Offset: -1}, {Offset: 9990, Limit: 20}, {Mode: SearchModeSemantic, Eligibility: groupedEligibility()}} {
			opts.Language, opts.EntityTypes = "en", types
			if _, trace, err := client.SearchWithTrace(ctx, "blue", opts); err == nil || trace.ErrorCategory != "validation" {
				t.Fatalf("%+v: err=%v trace=%+v", opts, err, trace)
			}
		}
	})

	t.Run("join contract", func(t *testing.T) {
		for name, join := range map[string]*Eligibility{
			"missing parent column": {SQL: `SELECT 0 AS priority`},
			"null parent":           {SQL: `SELECT NULL::text AS parent_id, 0 AS priority`},
			"reserved arg":          {SQL: groupedEligibilitySQL, Args: map[string]any{"traits": []string{}, "language": "x"}},
		} {
			if _, err := client.Search(ctx, "Not Guilty", SearchOptions{Language: "en", EntityTypes: types, Eligibility: join}); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		if _, err := client.Typeahead(ctx, "not", TypeaheadOptions{Language: "en", EntityTypes: types, Eligibility: &Eligibility{SQL: `SELECT NULL::text AS parent_id, 0 AS priority`}}); err == nil {
			t.Fatal("typeahead accepted null parent")
		}
	})

	t.Run("index plan with eligibility join", func(t *testing.T) {
		f.exec(`INSERT INTO host.items(id) SELECT 'filler-'||i FROM generate_series(1,50000) i;
INSERT INTO host.versions(id,item_id,language,live,is_default) SELECT 'fv-'||i,'filler-'||i,'zh',true,true FROM generate_series(1,50000) i;
INSERT INTO sk.search_documents(entity_type,entity_id,language,title,document,raw_document) SELECT 'item_version','fv-'||i,'zh','占位内容'||md5(i::text),'filler','filler' FROM generate_series(1,50000) i;
ANALYZE sk.search_documents; ANALYZE host.versions; ANALYZE host.items`)
		var trgm string
		if err := pool.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE extname='pg_trgm'`).Scan(&trgm); err != nil {
			t.Fatal(err)
		}
		qt := pgx.Identifier{trgm}.Sanitize()
		f.exec(`SET pg_trgm.word_similarity_threshold=0.1`)
		join := strings.ReplaceAll(groupedEligibilitySQL, "@traits::text[]", "'{}'::text[]")
		rows, err := pool.Query(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE,BUFFERS,COSTS OFF) SELECT sd.entity_id,e.parent_id FROM sk.search_documents sd
JOIN LATERAL (SELECT h.parent_id::text AS parent_id,h.priority::int AS priority FROM (%s) AS h LIMIT 1) e ON true
WHERE sd.language='zh' AND sk.searchkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.%%>) '鬼灭刃'
ORDER BY sk.searchkit_keyword_text(sd.title,sd.aliases,sd.keywords,sd.raw_document) OPERATOR(%s.<->>) '鬼灭刃' LIMIT 100`, join, qt, qt))
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
		t.Log(plan.String())
		if !strings.Contains(plan.String(), "search_documents_keyword_fuzzy") || strings.Contains(plan.String(), "Seq Scan") {
			t.Fatalf("expected indexed bounded fuzzy path with lateral join:\n%s", plan.String())
		}
		started := time.Now()
		expect(search("鬼灭刃", "zh", LanguageModeExact, SearchOptions{}), "g6=g6-zh-1/zh")
		t.Logf("grouped search over 50k joined documents: %s", time.Since(started))
	})
}
