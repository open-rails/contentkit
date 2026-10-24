package migrations_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestRemoveOffersPreservesBillingAndRefusesUnsettledIntent(t *testing.T) {
	for _, tt := range []struct {
		name             string
		revision, synced int
		product, price   string
		reject           bool
	}{
		{"pending", 2, 1, "product", "price", true},
		{"unlinked", 1, 1, "", "", true},
		{"unpriced", 1, 1, "product", "", true},
		{"settled", 1, 1, "product", "price", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			pool := pgtest.Pool(t, nil)
			schema := pgtest.EmptySchema(t, ctx, pool)
			pgtest.EnsureExtensions(t, ctx, pool)
			db := stdlib.OpenDBFromPool(pool)
			defer db.Close()
			steps, err := migratekit.Load(migrations.Postgres, ".")
			if err != nil {
				t.Fatal(err)
			}
			at := -1
			for i, s := range steps {
				if s.Name == "0040_remove_offers.up.sql" {
					at = i
					break
				}
			}
			if at < 0 {
				t.Fatal("missing migration")
			}
			runner := migratekit.NewPostgres(db, "contentkit").WithSchema(schema)
			if err = runner.ApplyMigrations(ctx, steps[:at]); err != nil {
				t.Fatal(err)
			}
			q := pgx.Identifier{schema}.Sanitize()
			// Stand-in billing-owned data must stay outside the removed local catalog.
			billing := pgtest.EmptySchema(t, ctx, pool)
			bq := pgx.Identifier{billing}.Sanitize()
			if _, err = pool.Exec(ctx, `CREATE TABLE `+bq+`.products(id text,price_id text,entitlement text); INSERT INTO `+bq+`.products VALUES ('product','price','tenant:content:item')`); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO `+q+`.content_offers(tenant_id,id,target_kind,target_id,terms,revision,synced_revision,product_id,price_id) VALUES ('tenant','offer','content','item','{"amount":"5.00","currency":"USD"}',$1,$2,$3,$4)`, tt.revision, tt.synced, tt.product, tt.price); err != nil {
				t.Fatal(err)
			}
			err = runner.ApplyMigrations(ctx, steps[:at+1])
			if tt.reject && (err == nil || !strings.Contains(err.Error(), "ContentKit offer removal:")) {
				t.Fatalf("unsettled offer not refused: %v", err)
			}
			if !tt.reject && err != nil {
				t.Fatal(err)
			}
			var exists bool
			if err = pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".content_offers").Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tt.reject {
				t.Fatalf("offer table exists=%v, rejected=%v", exists, tt.reject)
			}
			var product, price, key string
			if err = pool.QueryRow(ctx, `SELECT id,price_id,entitlement FROM `+bq+`.products`).Scan(&product, &price, &key); err != nil {
				t.Fatal(err)
			}
			if product != "product" || price != "price" || key != "tenant:content:item" {
				t.Fatal("billing-owned identity changed")
			}
		})
	}
}
