package contentkit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/taxonomy"
)

func TestMigrateOptionalTaxonomyIntegration(t *testing.T) {
	for _, separate := range []bool{false, true} {
		name := "shared_schema"
		if separate {
			name = "separate_search_schema"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := pgtest.Pool(t, nil)
			pgtest.EnsureExtensions(t, ctx, pool)
			host := pgtest.EmptySchema(t, ctx, pool)
			searchSchema := host
			cfg := MigrateConfig{Schema: host}
			if separate {
				searchSchema = pgtest.EmptySchema(t, ctx, pool)
				cfg.SearchSchema = searchSchema
			}
			db := stdlib.OpenDBFromPool(pool)
			defer db.Close()
			cfg.DB = db
			if err := Migrate(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, searchSchema+".content_nodes").Scan(&exists); err != nil || exists {
				t.Fatalf("unconfigured taxonomy unexpectedly exists: %v %v", exists, err)
			}
			cfg.Taxonomy = true
			if err := Migrate(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: searchSchema, Tenant: "site", Kinds: []string{"tag"}, Languages: []string{"en"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateNodes(ctx, []taxonomy.NodeInput{{TaxonomyID: "tag-1", Kind: "tag", Slug: "color", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Color"}}}}); err != nil {
				t.Fatalf("catalog unusable after unified migration: %v", err)
			}
			if err := Migrate(ctx, cfg); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+searchSchema+`.content_node_names WHERE taxonomy_id='tag-1' AND normalized='color'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("catalog lost on repeat: %d %v", count, err)
			}
			for _, table := range []string{host + ".social_comments", host + ".content_preference_snapshots", searchSchema + ".content_search_documents"} {
				if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
					t.Fatalf("missing plane %s: %v", table, err)
				}
			}
		})
	}
}
