package content

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
)

func TestMigrations_ChainIsHeaded(t *testing.T) {
	ms, err := migratekit.Load(migrations.Social, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(ms) != 4 || ms[3].Name != "0004_preference_snapshots.up.sql" {
		t.Fatalf("migrations = %+v", ms)
	}
}

// A pre-ContentKit installation (0001+0002 applied, entity_* rows present)
// converts in place: the rows keep their ids under tenant_id ” until
// AssignTenant stamps the host tenant, after which the runtime serves them.
func TestMigrate_ConvertsExistingRowsAndAssignsTenant(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	ms, err := migratekit.Load(migrations.Social, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, MigratekitApp).WithSchema(schema).ApplyMigrations(ctx, ms[:2]); err != nil {
		t.Fatalf("apply pre-cut lineage: %v", err)
	}
	q := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, "SET search_path TO "+schema+", public; "+sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`INSERT INTO social_reactions (entity_type, entity_id, user_id, value) VALUES ('gallery', '42:en', 'u1', 1)`)
	q(`INSERT INTO social_favorites (user_id, entity_type, entity_id) VALUES ('u1', 'gallery', '42:en')`)
	q(`INSERT INTO social_comments (entity_type, entity_id, user_id, body) VALUES ('gallery', '42:en', 'u1', 'old thread')`)
	q(`INSERT INTO social_entity_counts (entity_type, entity_id, likes, favorites, comment_count) VALUES ('gallery', '42:en', 1, 1, 1)`)
	q(`INSERT INTO social_posts (author_id, title, body, is_draft) VALUES ('u1', 'old post', 'b', false)`)
	q(`INSERT INTO social_poll_questions (question) VALUES ('old poll?')`)

	if err := Migrate(ctx, db, schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := Migrate(ctx, db, schema); err != nil {
		t.Fatalf("second migrate (idempotent): %v", err)
	}
	var unassigned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.social_reactions WHERE tenant_id = '' AND content_kind = 'gallery' AND content_id = '42:en' AND content_version_id = ''`).Scan(&unassigned); err != nil || unassigned != 1 {
		t.Fatalf("converted reaction rows = %d err=%v, want 1 under tenant ''", unassigned, err)
	}
	n, err := AssignTenant(ctx, pool, schema, "doujins")
	if err != nil || n != 6 {
		t.Fatalf("AssignTenant = %d err=%v, want 6 rows", n, err)
	}
	if n, err := AssignTenant(ctx, pool, schema, "doujins"); err != nil || n != 0 {
		t.Fatalf("second AssignTenant = %d err=%v, want 0 (idempotent)", n, err)
	}
	res := &fakeResolver{}
	res.set("gallery", "42:en", true, true)
	rt, err := New(ctx, Options{Pool: pool, Schema: schema, Tenant: "doujins", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: res, ContentKinds: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	g := rt.Ref("gallery", "42:en")
	if c := countsOf(t, rt, g); c.Likes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("converted counts = %+v", c)
	}
	if list, err := rt.comments.list(ctx, Actor{ID: "u1"}, "gallery", "42:en", "", 10, 0); err != nil || len(list) != 1 || list[0].Body != "old thread" {
		t.Fatalf("converted thread = %+v err=%v", list, err)
	}
	if m, _ := rt.MyReactions(ctx, Actor{ID: "u1"}, []contentref.ContentRef{g}); m[g.Key()] != 1 {
		t.Fatalf("converted reaction = %v", m)
	}
	if polls, _ := rt.polls.list(ctx, Actor{ID: "u1"}, listFilter{limit: 10}); len(polls) != 1 {
		t.Fatalf("converted polls = %v", polls)
	}
	// An unmigrated schema is refused at construction.
	if _, err := New(ctx, Options{Pool: pool, Schema: schema + "_missing", Tenant: "doujins", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: res}); err == nil {
		t.Fatal("New accepted a schema without the social lineage")
	}
}
