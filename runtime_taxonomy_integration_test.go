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
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+q+"; CREATE TABLE "+q+".host_entities(tenant_id text, id text, PRIMARY KEY(tenant_id,id))"); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	cfg := MigrateConfig{DB: db, Schema: schema}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: "site", Kinds: []string{"tag"}, Languages: []string{"en"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateNodes(ctx, []taxonomy.NodeInput{{TaxonomyID: "tag-1", Kind: "tag", Slug: "color", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Color"}}}}); err != nil {
		t.Fatal(err)
	}
	// Host IDs are opaque by default. A host may add its own tenant-scoped FK
	// when content assignments and host rows share the same lifecycle.
	if _, err := pool.Exec(ctx, "ALTER TABLE "+q+".content_assignments ADD CONSTRAINT host_content_fk FOREIGN KEY(tenant_id,content_id) REFERENCES "+q+".host_entities(tenant_id,id); INSERT INTO "+q+".host_entities VALUES('site','host-1'),('other','foreign-1')"); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO " + q + ".content_assignments(tenant_id,content_kind,content_id,taxonomy_id,relation) VALUES('site','gallery',$1,$2,'tag')"
	if _, err := pool.Exec(ctx, insert, "host-1", "tag-1"); err != nil {
		t.Fatalf("valid host and internal foreign keys: %v", err)
	}
	for _, refs := range [][2]string{{"foreign-1", "tag-1"}, {"host-1", "missing-node"}} {
		_, err := pool.Exec(ctx, insert, refs[0], refs[1])
		if pgerr, ok := err.(interface{ SQLState() string }); !ok || pgerr.SQLState() != "23503" {
			t.Fatalf("foreign keys must reject cross-tenant host or missing taxonomy: %v", err)
		}
	}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("repeat baseline: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+q+".content_node_names WHERE taxonomy_id='tag-1' AND normalized='color'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("catalog preserved on rerun: %d %v", count, err)
	}
	for _, table := range []string{"host_entities", "social_comments", "social_posts", "content_preference_snapshots", "content_search_documents", "content_nodes"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, table}.Sanitize()).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing same-schema table %s: %v", table, err)
		}
	}
}
