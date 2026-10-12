package contentkit

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/migrations"
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
	store, err := groups.New(groups.Options{Pool: pool, Schema: schema, Tenant: "site", Kinds: kinds.Config{Languages: []string{"en"},
		Kinds:      map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}}},
		GroupKinds: map[string]kinds.GroupKind{"tag": {Route: "tags"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroups(ctx, []groups.GroupInput{{ID: groups.GroupID(grp(1)), Kind: "tag", Slug: "color", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Color"}}}}); err != nil {
		t.Fatal(err)
	}
	// Memberships reference ContentKit's own content, within the tenant.
	if _, err := pool.Exec(ctx, "INSERT INTO "+q+".content_items(tenant_id,id,kind) VALUES('site',$1,'gallery'),('other',$2,'gallery')", cid(1), cid(2)); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO " + q + ".content_memberships(tenant_id,group_id,content_id) VALUES('site',$1,$2)"
	if _, err := pool.Exec(ctx, insert, grp(1), cid(1)); err != nil {
		t.Fatalf("valid memberships: %v", err)
	}
	for _, refs := range [][2]string{{grp(1), cid(2)}, {grp(2), cid(1)}} {
		_, err := pool.Exec(ctx, insert, refs[0], refs[1])
		if pgerr, ok := err.(interface{ SQLState() string }); !ok || pgerr.SQLState() != "23503" {
			t.Fatalf("foreign keys must reject cross-tenant content or a missing group: %v", err)
		}
	}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	chain, err := fs.Glob(migrations.Postgres, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM public.migrations WHERE app='contentkit' AND schema=$1", schema).Scan(&count); err != nil || count != len(chain) {
		t.Fatalf("migration ledger rows: %d, want %d %v", count, len(chain), err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+q+".content_group_names WHERE group_id=$1 AND normalized='color'", grp(1)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("groups preserved on rerun: %d %v", count, err)
	}
	for _, table := range []string{"host_entities", "content_comments", "content_items", "content_facets", "content_preference_sync", "content_search_documents", "content_search_invalid", "content_groups", "content_media_releases"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, table}.Sanitize()).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing same-schema table %s: %v", table, err)
		}
	}
	for _, table := range []string{"content_media_slots", "content_media_slot_backfill", "content_nodes", "content_node_names", "content_edges", "content_assignments", "content_node_counts", "content_posts"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, table}.Sanitize()).Scan(&exists); err != nil || exists {
			t.Fatalf("a retired table survives: %s: %v", table, err)
		}
	}
}
