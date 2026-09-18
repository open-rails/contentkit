package contentkit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

// schemaFingerprint renders every ContentKit-owned object of schema app:
// columns, constraints, indexes, triggers and functions (extension objects
// excluded). Both Postgres lineages must converge on one fingerprint.
func schemaFingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'column '||table_name||'.'||column_name||' '||data_type||' null='||is_nullable||' default='||coalesce(column_default,'') FROM information_schema.columns WHERE table_schema='app' ORDER BY 1`,
		`SELECT 'constraint '||conrelid::regclass||' '||conname||' '||pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='app'::regnamespace ORDER BY 1`,
		`SELECT 'index '||indexdef FROM pg_indexes WHERE schemaname='app' ORDER BY 1`,
		`SELECT 'trigger '||tgrelid::regclass||' '||tgname||' '||pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN (SELECT oid FROM pg_class WHERE relnamespace='app'::regnamespace) ORDER BY 1`,
		`SELECT 'function '||proname||'('||pg_get_function_identity_arguments(p.oid)||') '||md5(pg_get_functiondef(p.oid)) FROM pg_proc p WHERE pronamespace='app'::regnamespace AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e') ORDER BY 1`,
		`SELECT 'sequence '||sequencename FROM pg_sequences WHERE schemaname='app' ORDER BY 1`,
	} {
		rows, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// Both lineages end on the keyword profile schema (3 tables, 28 columns, no
// embedding tables); the legacy lineage converts existing rows in place and
// drops the embedding tables; the worker and search run identically on each.
func TestKeywordProfileIntegration(t *testing.T) {
	fingerprints := map[string]string{}
	for _, profile := range []struct {
		name  string
		files fs.FS
	}{{"keyword", migrations.Postgres}, {"legacy", migrations.LegacyPostgres}} {
		t.Run(profile.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			pool := profileDB(t, ctx, "ck_profile_"+profile.name, nil)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			applyLineage(t, ctx, pool, profile.files, func(name string) {
				if profile.name != "legacy" || name != "0004_content_refs.up.sql" {
					return
				}
				// Data on the pre-ContentKit schema: a vector, an old-keyed document
				// and dirty row. The vector table is exported before this step
				// (docs/migration.md); the rows convert in place under tenant ''.
				exec(`INSERT INTO app.embedding_vectors(entity_type,entity_id,model,language,embedding) VALUES('gallery','1','preserved','en','[1,2,3]'::app.halfvec)`)
				exec(`INSERT INTO app.search_documents(entity_type,entity_id,language,title,raw_document,document) VALUES('gallery','old','en','Old Row','Old Row','old row')`)
				exec(`INSERT INTO app.search_dirty(entity_type,entity_id,language) VALUES('gallery','old','en')`)
			})
			snapshot := func() string {
				var s string
				if err := pool.QueryRow(ctx, "SELECT string_agg(name||':'||checksum,',' ORDER BY name) FROM public.applied_migrations").Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			ledger := snapshot()
			var tables, columns, embedding int
			if err := pool.QueryRow(ctx, "SELECT count(DISTINCT table_name),count(*) FROM information_schema.columns WHERE table_schema='app' AND table_name IN ('content_search_documents','content_search_dirty','content_search_backfill')").Scan(&tables, &columns); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='app' AND (table_name LIKE 'embedding%' OR table_name LIKE 'search\\_%')").Scan(&embedding); err != nil {
				t.Fatal(err)
			}
			if tables != 3 || columns != 28 || embedding != 0 {
				t.Fatalf("schema=%d tables/%d columns, %d pre-ContentKit tables; want 3/28/0", tables, columns, embedding)
			}
			var vector bool
			if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')").Scan(&vector); err != nil {
				t.Fatal(err)
			}
			if vector != (profile.name == "legacy") {
				t.Fatalf("vector extension present=%v", vector)
			}
			fingerprints[profile.name] = schemaFingerprint(t, ctx, pool)

			client, err := NewClient(ClientConfig{Pool: pool, Schema: "app", Tenant: testTenant})
			if err != nil {
				t.Fatal(err)
			}
			count := func(table, where string) int {
				t.Helper()
				var n int
				if err := pool.QueryRow(ctx, "SELECT count(*) FROM app."+table+" WHERE "+where).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			expectHits := func(query string, want int) {
				t.Helper()
				page, err := client.Search(ctx, query, SearchOptions{ContentKinds: []string{"gallery"}})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Hits) != want {
					t.Fatalf("%q got %v want %d hits", query, page, want)
				}
			}
			if profile.name == "legacy" {
				// Converted rows carry tenant '' and are unreachable by any tenant;
				// the documented cleanup removes them.
				if count("content_search_documents", "tenant_id='' AND content_id='old' AND content_version_id=''") != 1 || count("content_search_dirty", "tenant_id=''") != 1 {
					t.Fatal("pre-ContentKit rows must survive conversion under tenant ''")
				}
				expectHits("Old Row", 0)
				exec(`DELETE FROM app.content_search_documents WHERE tenant_id=''; DELETE FROM app.content_search_dirty WHERE tenant_id=''; DELETE FROM app.content_search_backfill WHERE tenant_id=''`)
				if count("content_search_documents", "true") != 0 {
					t.Fatal("cleanup left rows")
				}
			}

			documents := map[string]string{"1": "Blue ocean"}
			var buildErr error
			opts := worker.Options{Pool: pool, Schema: "app", Tenant: testTenant, SupportedLanguages: []string{"en"}, ContentKinds: []string{"gallery"},
				ListContent: func(context.Context, string, string, string, string, int) ([]ContentRef, string, bool, error) {
					return nil, "", true, nil
				},
				BuildKeywordDocuments: func(_ context.Context, _, kind, lang string, refs []ContentRef) ([]KeywordDocument, error) {
					if buildErr != nil {
						return nil, buildErr
					}
					var out []KeywordDocument
					for _, ref := range refs {
						if title, ok := documents[ref.ContentID]; ok {
							out = append(out, KeywordDocument{DocumentKey: DocumentKey{ContentRef: ref, Language: lang}, Title: title, Aliases: []string{"azure waves"}})
						}
					}
					return out, nil
				}}
			mark := func(deleted bool) {
				t.Helper()
				if err := search.MarkDirty(ctx, pool, "app", []search.DirtyMark{{DocumentKey: DocumentKey{ContentRef: gallery("1"), Language: "en"}, Deleted: deleted}}); err != nil {
					t.Fatal(err)
				}
				if err := worker.SyncOnce(ctx, opts); err != nil {
					t.Fatal(err)
				}
			}
			mark(false)
			expectHits("Blue ocean", 1)
			expectHits("azure", 1)
			buildErr = errors.New("temporary source failure")
			if err := search.MarkDirty(ctx, pool, "app", []search.DirtyMark{{DocumentKey: DocumentKey{ContentRef: gallery("1"), Language: "en"}, Reason: "retry"}}); err != nil {
				t.Fatal(err)
			}
			if err := worker.SyncOnce(ctx, opts); !errors.Is(err, buildErr) {
				t.Fatalf("expected builder failure, got %v", err)
			}
			expectHits("Blue ocean", 1)
			if count("content_search_dirty", "true") != 1 {
				t.Fatal("failed build lost pending retry")
			}
			buildErr = nil
			documents["1"] = "Red forest"
			mark(false)
			expectHits("Red forest", 1)
			expectHits("Blue ocean", 0)
			// Omitting a requested reference means the content no longer exists.
			delete(documents, "1")
			mark(false)
			expectHits("Red forest", 0)
			documents["1"] = "Amber field"
			mark(false)
			expectHits("Amber field", 1)
			mark(true)
			expectHits("Amber field", 0)
			if snapshot() != ledger {
				t.Fatal("keyword runtime modified the migration ledger")
			}
			_ = fmt.Sprint
		})
	}
	if fingerprints["keyword"] == "" || fingerprints["legacy"] == "" {
		t.Skip("both lineages must run")
	}
	if fingerprints["keyword"] != fingerprints["legacy"] {
		t.Fatalf("lineages diverge:\n--- keyword ---\n%s\n--- legacy ---\n%s", fingerprints["keyword"], fingerprints["legacy"])
	}
}
