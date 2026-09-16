package searchkit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/searchkit/migrations"
	"github.com/open-rails/searchkit/pg"
	"github.com/open-rails/searchkit/runtime"
	"github.com/open-rails/searchkit/worker"
)

// These tests require pg_trgm + PGroonga, and vector for the existing combined
// installation subtest. Every test owns a new database, never a shared schema.
func TestKeywordProfileIntegration(t *testing.T) {
	dsn := os.Getenv("SEARCHKIT_PROFILE_URL")
	if dsn == "" {
		t.Skip("SEARCHKIT_PROFILE_URL not set (PGroonga + vector server)")
	}
	for _, profile := range []struct {
		name            string
		files           fs.FS
		tables, columns int
	}{
		{"fresh_keyword", migrations.KeywordPostgres, 3, 25},
		{"existing_combined", migrations.Postgres, 8, 64},
	} {
		t.Run(profile.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			admin, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			db := fmt.Sprintf("sk_profile_%d", time.Now().UnixNano())
			if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{db}.Sanitize()) }()
			cfg := admin.Config()
			cfg.ConnConfig.Database = db
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			// The fixture ledger records the exact migration names/checksums the host
			// applied. Runtime operations must neither rewrite it nor change profiles.
			if _, err := pool.Exec(ctx, `CREATE SCHEMA app; CREATE TABLE public.applied_migrations(name text PRIMARY KEY,checksum text NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			files, err := fs.ReadDir(profile.files, ".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range files {
				sql, err := fs.ReadFile(profile.files, entry.Name())
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
					_, err = tx.Exec(ctx, "INSERT INTO public.applied_migrations VALUES($1,$2)", entry.Name(), fmt.Sprintf("%x", sha256.Sum256(sql)))
				}
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("%s: %v", entry.Name(), err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() string {
				var s string
				err := pool.QueryRow(ctx, "SELECT string_agg(name||':'||checksum,',' ORDER BY name) FROM public.applied_migrations").Scan(&s)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			ledger := snapshot()
			var tables, columns int
			if err := pool.QueryRow(ctx, "SELECT count(DISTINCT table_name),count(*) FROM information_schema.columns WHERE table_schema='app' AND table_name IN ('search_documents','search_dirty','search_documents_backfill_state','embedding_models','embedding_tasks','embedding_vectors','embedding_vectors_backfill_state','embedding_dead_letters')").Scan(&tables, &columns); err != nil {
				t.Fatal(err)
			}
			if tables != profile.tables || columns != profile.columns {
				t.Fatalf("schema=%d/%d want %d/%d", tables, columns, profile.tables, profile.columns)
			}
			if profile.name == "fresh_keyword" {
				var vector bool
				if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')").Scan(&vector); err != nil {
					t.Fatal(err)
				}
				if vector {
					t.Fatal("keyword profile installed vector")
				}
			}
			if profile.name == "existing_combined" {
				if _, err := pool.Exec(ctx, `INSERT INTO app.embedding_vectors(entity_type,entity_id,model,language,embedding) VALUES('gallery','1','preserved','en','[1,2,3]'::app.halfvec)`); err != nil {
					t.Fatal(err)
				}
			}
			documents := map[string]string{"1": "Blue ocean"}
			var buildErr error
			runtimeOpts := runtime.Options{Pool: pool, Schema: "app", BuildLexicalString: func(_ context.Context, _ string, _ string, ids []string) (map[string]string, error) {
				if buildErr != nil {
					return nil, buildErr
				}
				out := map[string]string{}
				for _, id := range ids {
					if text, ok := documents[id]; ok {
						out[id] = text
					}
				}
				return out, nil
			}}
			if profile.name == "fresh_keyword" {
				runtimeOpts.BuildKeywordDocuments = func(ctx context.Context, kind, lang string, ids []string) (map[string]pg.KeywordDocument, error) {
					docs, err := runtimeOpts.BuildLexicalString(ctx, kind, lang, ids)
					if err != nil {
						return nil, err
					}
					structured := make(map[string]pg.KeywordDocument, len(docs))
					for id, title := range docs {
						structured[id] = pg.KeywordDocument{Title: title, Aliases: []string{"azure waves"}}
					}
					return structured, nil
				}
			}
			rt, err := runtime.New(runtimeOpts)
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(ClientConfig{Pool: pool, Schema: "app"})
			if err != nil {
				t.Fatal(err)
			}
			opts := worker.SearchkitOptions{Pool: pool, Schema: "app", SupportedLanguages: []string{"en"}, LexicalEntityTypes: []string{"gallery"}, ListEntityIDsPage: func(context.Context, string, string, string, int) ([]string, string, bool, error) {
				return nil, "", true, nil
			}}
			mark := func(deleted bool) {
				t.Helper()
				_, err := pool.Exec(ctx, `INSERT INTO app.search_dirty(entity_type,entity_id,language,is_deleted) VALUES('gallery','1','en',$1) ON CONFLICT(entity_type,entity_id,language) DO UPDATE SET is_deleted=EXCLUDED.is_deleted`, deleted)
				if err != nil {
					t.Fatal(err)
				}
				if err := worker.SyncOnce(ctx, rt, opts); err != nil {
					t.Fatal(err)
				}
			}
			search := func(query string, want int) {
				t.Helper()
				hits, err := client.Search(ctx, query, SearchOptions{EntityTypes: []string{"gallery"}})
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) != want {
					t.Fatalf("%q got %v want %d hits", query, hits, want)
				}
			}
			mark(false)
			search("Blue ocean", 1)
			if profile.name == "fresh_keyword" {
				search("azure", 1)
			}
			buildErr = errors.New("temporary source failure")
			if _, err := pool.Exec(ctx, `INSERT INTO app.search_dirty(entity_type,entity_id,language) VALUES('gallery','1','en') ON CONFLICT(entity_type,entity_id,language) DO UPDATE SET reason='retry'`); err != nil {
				t.Fatal(err)
			}
			if err := worker.SyncOnce(ctx, rt, opts); !errors.Is(err, buildErr) {
				t.Fatalf("expected builder failure, got %v", err)
			}
			search("Blue ocean", 1)
			var pending int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM app.search_dirty").Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 1 {
				t.Fatal("failed build lost pending retry")
			}
			buildErr = nil
			documents["1"] = "Red forest"
			mark(false)
			search("Red forest", 1)
			search("Blue ocean", 0)
			// Omitting a requested ID means the source entity no longer exists.
			delete(documents, "1")
			mark(false)
			search("Red forest", 0)
			documents["1"] = "Amber field"
			mark(false)
			search("Amber field", 1)
			mark(true)
			search("Amber field", 0)
			if snapshot() != ledger {
				t.Fatal("keyword runtime modified the migration ledger")
			}
			if profile.name == "existing_combined" {
				var value string
				if err := pool.QueryRow(ctx, `SELECT embedding::text FROM app.embedding_vectors WHERE model='preserved'`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				if value != "[1,2,3]" {
					t.Fatal("optional semantic data changed", value)
				}
			}
		})
	}
}
