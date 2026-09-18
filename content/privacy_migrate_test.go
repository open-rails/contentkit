package content

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/migratekit"
)

func TestPrivateMigrationPreservesLegacyPublishedContent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	ms, err := migratekit.Load(migrations.Social, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, MigratekitApp).WithSchema(schema).ApplyMigrations(ctx, ms[:4]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.social_comments (tenant_id,content_kind,content_id,user_id,body) VALUES ('hostapp','gallery','1','u1','legacy publication')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	rt, err := New(ctx, Options{Pool: pool, Schema: schema, Tenant: testTenant, Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	var body, user string
	var published *string
	if err := pool.QueryRow(ctx, `SELECT body,published_body,user_id FROM `+schema+`.social_comments`).Scan(&body, &published, &user); err != nil {
		t.Fatal(err)
	}
	if body != "legacy publication" || published != nil || user != "u1" {
		t.Fatalf("legacy published data altered: %q %v %q", body, published, user)
	}
}
