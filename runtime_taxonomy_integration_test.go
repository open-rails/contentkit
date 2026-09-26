package contentkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/taxonomy"
)

func TestMigrateHostSchemaAndForeignKeysIntegration(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	// Mixed case requires identifier quoting in DDL, lookup and runtime SQL.
	schema := fmt.Sprintf("HostContent_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE schema=$1", schema)
	})
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	cfg := MigrateConfig{DB: db, Schema: schema}
	// Both first callers target a missing schema; creation and migrations
	// must serialize under the same migration lock.
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- Migrate(ctx, cfg) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent first initialization: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE "+q+".host_entities(tenant_id text, id text, PRIMARY KEY(tenant_id,id))"); err != nil {
		t.Fatal(err)
	}
	store, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: "site", Kinds: []string{"tag"}, Languages: []string{"en"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateNodes(ctx, []taxonomy.NodeInput{{TaxonomyID: taxonomy.TaxonomyID(tax(1)), Kind: "tag", Slug: "color", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Color"}}}}); err != nil {
		t.Fatal(err)
	}
	// Host IDs are opaque by default. A host may add its own tenant-scoped FK
	// when content assignments and host rows share the same lifecycle.
	if _, err := pool.Exec(ctx, "ALTER TABLE "+q+".content_assignments ADD CONSTRAINT host_content_fk FOREIGN KEY(tenant_id,content_id) REFERENCES "+q+".host_entities(tenant_id,id); INSERT INTO "+q+".host_entities VALUES('site','"+cid(1)+"'),('other','"+cid(2)+"')"); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO " + q + ".content_assignments(tenant_id,content_kind,content_id,taxonomy_id,relation) VALUES('site','gallery',$1,$2,'tag')"
	if _, err := pool.Exec(ctx, insert, cid(1), tax(1)); err != nil {
		t.Fatalf("valid host and internal foreign keys: %v", err)
	}
	for _, refs := range [][2]string{{cid(2), tax(1)}, {cid(1), tax(2)}} {
		_, err := pool.Exec(ctx, insert, refs[0], refs[1])
		if pgerr, ok := err.(interface{ SQLState() string }); !ok || pgerr.SQLState() != "23503" {
			t.Fatalf("foreign keys must reject cross-tenant host or missing taxonomy: %v", err)
		}
	}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.migrations WHERE app='contentkit' AND schema=$1", schema).Scan(&count); err != nil || count != 2 {
		t.Fatalf("two migration ledger rows: %d %v", count, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+q+".content_node_names WHERE taxonomy_id=$1 AND normalized='color'", tax(1)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("catalog preserved on rerun: %d %v", count, err)
	}
	for _, table := range []string{"host_entities", "content_comments", "content_posts", "content_preference_sync", "content_search_documents", "content_search_invalid", "content_nodes"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, table}.Sanitize()).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing same-schema table %s: %v", table, err)
		}
	}
}
