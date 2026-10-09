package contenturl_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/taxonomy"
)

const tenant = "doujins"

var codeRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{9}$`)

func id(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

func ref(kind string, n int) contentref.ContentRef { return contentref.New(tenant, kind, id(n)) }

func setup(t *testing.T) (context.Context, *pgxpool.Pool, string, *contenturl.Store) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	return ctx, pool, schema, newStore(t, pool, schema, tenant)
}

func newStore(t *testing.T, pool *pgxpool.Pool, schema, tenant string) *contenturl.Store {
	t.Helper()
	s, err := contenturl.New(contenturl.Options{Pool: pool, Schema: schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *contenturl.Store, entries ...contenturl.Entry) []contenturl.Link {
	t.Helper()
	links, err := s.Put(context.Background(), entries...)
	if err != nil {
		t.Fatal(err)
	}
	return links
}

func TestGeneratorIsUniformAndUnique(t *testing.T) {
	ctx, pool, schema, _ := setup(t)
	const n = 32000
	rows, err := pool.Query(ctx, `SELECT `+schema+`.contentkit_content_code() FROM generate_series(1, $1)`, n)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var counts [contenturl.CodeLength][128]int
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		if !codeRE.MatchString(c) {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
		for i := 0; i < len(c); i++ {
			counts[i][c[i]]++
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(seen) != n {
		t.Fatalf("%d distinct codes of %d", len(seen), n)
	}
	// 1000 expected per symbol and position; 5 standard deviations is ±155.
	for i := range counts {
		for j := 0; j < len(contenturl.Alphabet); j++ {
			if c := counts[i][contenturl.Alphabet[j]]; c < 845 || c > 1155 {
				t.Errorf("position %d symbol %c: %d of %d", i, contenturl.Alphabet[j], c, n)
			}
		}
	}
}

func TestPutAssignsOnceAndResolves(t *testing.T) {
	ctx, _, _, s := setup(t)
	video, gallery := ref("video", 1), ref("gallery", 1) // same id, different kinds
	links := put(t, s,
		contenturl.Entry{ContentRef: video, Title: "Night Before the Counteroffensive", Titles: map[string]string{"es": "La Noche Antes", "JA": "前夜"}},
		contenturl.Entry{ContentRef: gallery, Title: "A Gallery"},
	)
	v, g := links[0], links[1]
	if !codeRE.MatchString(string(v.Code)) || !codeRE.MatchString(string(g.Code)) || v.Code == g.Code {
		t.Fatalf("codes %q %q", v.Code, g.Code)
	}
	if v.Slug != "night-before-the-counteroffensive" || v.Slugs["es"] != "la-noche-antes" || len(v.Slugs) != 1 || !v.ContentRef.Equal(video) {
		t.Fatalf("video link %+v", v)
	}

	// Renames keep the code; Titles nil keeps the localized slugs, empty clears them.
	again := put(t, s, contenturl.Entry{ContentRef: video, Title: "Renamed"})[0]
	if again.Code != v.Code || again.Slug != "renamed" || again.Slugs["es"] != "la-noche-antes" {
		t.Fatalf("rename %+v", again)
	}
	cleared := put(t, s, contenturl.Entry{ContentRef: video, Title: "Renamed", Titles: map[string]string{}})[0]
	if cleared.Code != v.Code || cleared.Slugs != nil {
		t.Fatalf("cleared %+v", cleared)
	}

	lower, err := contenturl.ParseCode(strings.ToLower(string(v.Code)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(ctx, lower)
	if err != nil || got.Code != v.Code || got.Slug != "renamed" || !got.ContentRef.Equal(video) {
		t.Fatalf("Resolve %+v %v", got, err)
	}
	if _, err := s.Resolve(ctx, "00000000A"); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("unknown code: %v", err)
	}
	if _, err := s.Resolve(ctx, "g4vrq3zq5"); !errors.Is(err, contenturl.ErrInvalidCode) {
		t.Fatalf("non-canonical code: %v", err)
	}

	byKey, err := s.Links(ctx, []contentref.ContentRef{video, gallery, ref("video", 99), video.WithVersion("v2")})
	if err != nil {
		t.Fatal(err)
	}
	if len(byKey) != 3 || byKey[video.Key()].Code != v.Code || byKey[gallery.Key()].Code != g.Code || byKey[video.WithVersion("v2").Key()].Code != v.Code {
		t.Fatalf("Links %+v", byKey)
	}
}

func TestPutRefusesInvalidInput(t *testing.T) {
	_, _, _, s := setup(t)
	for _, e := range []contenturl.Entry{
		{ContentRef: contentref.New("hentai0", "video", id(1))},
		{ContentRef: contentref.New(tenant, "video", "42")},
		{ContentRef: contentref.New(tenant, "video", strings.ToUpper("0192000a-0000-7000-8000-00000000000b"))},
		{ContentRef: ref("video", 1).WithVersion("v1")},
		{ContentRef: ref("video", 1), Titles: map[string]string{"e s": "x"}},
	} {
		if _, err := s.Put(context.Background(), e); !errors.Is(err, contenturl.ErrInvalid) {
			t.Errorf("Put(%#v) = %v", e, err)
		}
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	other := newStore(t, pool, schema, "hentai0")
	mine := put(t, s, contenturl.Entry{ContentRef: ref("video", 1), Title: "Mine"})[0]
	theirs := put(t, other, contenturl.Entry{ContentRef: contentref.New("hentai0", "video", id(1)), Title: "Theirs"})[0]
	if mine.Code == theirs.Code {
		t.Fatal("independent tenants drew one code")
	}
	if _, err := other.Resolve(ctx, mine.Code); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("hentai0 resolved a doujins code: %v", err)
	}
	if got, err := other.Resolve(ctx, theirs.Code); err != nil || got.TenantID != "hentai0" || got.Slug != "theirs" {
		t.Fatalf("own code %+v %v", got, err)
	}
}

// A generator that keeps returning a taken code exercises the retry.
func TestPutRetriesCollisions(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	taken := put(t, s, contenturl.Entry{ContentRef: ref("video", 1), Title: "first"})[0].Code
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s.collide;
CREATE OR REPLACE FUNCTION %[1]s.contentkit_content_code() RETURNS text LANGUAGE sql VOLATILE AS $$
 SELECT CASE WHEN nextval('%[1]s.collide') <= 3 THEN '%[2]s' ELSE 'ZZZZZZZZ' || (currval('%[1]s.collide') %% 10)::text END $$`, schema, taken)); err != nil {
		t.Fatal(err)
	}
	links := put(t, s, contenturl.Entry{ContentRef: ref("video", 2), Title: "second"}, contenturl.Entry{ContentRef: ref("video", 3), Title: "third"})
	if links[0].Code == taken || links[1].Code == taken || links[0].Code == links[1].Code {
		t.Fatalf("collision retry gave %q %q (taken %q)", links[0].Code, links[1].Code, taken)
	}
	// A generator stuck on taken codes fails instead of looping.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.contentkit_content_code() RETURNS text LANGUAGE sql VOLATILE AS $$ SELECT '%s' $$`, schema, taken)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, contenturl.Entry{ContentRef: ref("video", 4)}); err == nil || !strings.Contains(err.Error(), "unassigned") {
		t.Fatalf("stuck generator: %v", err)
	}
}

func TestConcurrentPutsAgreeOnOneCode(t *testing.T) {
	ctx, pool, _, s := setup(t)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]contenturl.Code, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback(ctx)
			links, err := s.WithTx(tx).Put(ctx, contenturl.Entry{ContentRef: ref("video", 1), Title: fmt.Sprint("t", i)})
			if err == nil {
				codes[i] = links[0].Code
				err = tx.Commit(ctx)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if errs[i] != nil || codes[i] != codes[0] {
			t.Fatalf("put %d: %q %v (first %q)", i, codes[i], errs[i], codes[0])
		}
	}
}

func TestSQLTransactionRollsBack(t *testing.T) {
	ctx, pool, _, s := setup(t)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	link, err := s.WithSQLTx(tx).Put(ctx, contenturl.Entry{ContentRef: ref("video", 1), Title: "draft", Titles: map[string]string{"es": "borrador"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.WithSQLTx(tx).Resolve(ctx, link[0].Code); err != nil || got.Slugs["es"] != "borrador" {
		t.Fatalf("in-tx resolve %+v %v", got, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, link[0].Code); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("rolled-back code resolves: %v", err)
	}
}

func TestMergeRedirectsOneHop(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	links := put(t, s,
		contenturl.Entry{ContentRef: ref("gallery", 1), Title: "a"},
		contenturl.Entry{ContentRef: ref("gallery", 2), Title: "b"},
		contenturl.Entry{ContentRef: ref("gallery", 3), Title: "c"},
	)
	a, b, c := links[0], links[1], links[2]
	if err := s.Merge(ctx, ref("gallery", 1), ref("gallery", 2)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Resolve(ctx, a.Code); got.Code != b.Code || !got.ContentRef.Equal(ref("gallery", 2)) {
		t.Fatalf("a resolves to %+v", got)
	}
	if err := s.Merge(ctx, ref("gallery", 2), ref("gallery", 1)); !errors.Is(err, contenturl.ErrConflict) {
		t.Fatalf("merge back: %v", err)
	}
	if err := s.Merge(ctx, ref("gallery", 2), ref("gallery", 3)); err != nil {
		t.Fatal(err)
	}
	var merged string
	if err := pool.QueryRow(ctx, `SELECT merged_into FROM `+schema+`.content_codes WHERE code = $1`, string(a.Code)).Scan(&merged); err != nil || merged != string(c.Code) {
		t.Fatalf("a.merged_into = %q %v, want %q (flattened)", merged, err, c.Code)
	}
	for _, code := range []contenturl.Code{a.Code, b.Code, c.Code} {
		if got, err := s.Resolve(ctx, code); err != nil || got.Code != c.Code || got.Slug != "c" {
			t.Fatalf("%s resolves to %+v %v", code, got, err)
		}
	}
	if byKey, _ := s.Links(ctx, []contentref.ContentRef{ref("gallery", 1)}); byKey[ref("gallery", 1).Key()].Code != c.Code {
		t.Fatalf("Links of a merged ref %+v", byKey)
	}
	if err := s.Merge(ctx, ref("gallery", 9), ref("gallery", 3)); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("unregistered merge: %v", err)
	}
}

func TestAliases(t *testing.T) {
	ctx, pool, _, s := setup(t)
	links := put(t, s, contenturl.Entry{ContentRef: ref("gallery", 1), Title: "one"}, contenturl.Entry{ContentRef: ref("gallery", 2), Title: "two"})
	aliases := []contenturl.Alias{
		{Source: "doujins-legacy", LegacyKind: "folder", Key: "12345", ContentRef: ref("gallery", 1)},
		{Source: "doujins-legacy", LegacyKind: "folder-token", Key: "ab12cd34", ContentRef: ref("gallery", 1)},
		{Source: "doujins-legacy", LegacyKind: "object-token", Key: "zz99yy88", Locator: "7", ContentRef: ref("gallery", 1)},
		{Source: "doujins-legacy", LegacyKind: "tag-name", Key: "Mother X Son", ContentRef: ref("gallery", 2)},
	}
	if err := s.PutAliases(ctx, aliases...); err != nil {
		t.Fatal(err)
	}
	// An import re-run repeats its aliases: a no-op.
	if err := s.PutAliases(ctx, aliases...); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	if got, err := s.ResolveAlias(ctx, "doujins-legacy", "folder", "12345"); err != nil || got.Code != links[0].Code || got.Locator != "" {
		t.Fatalf("alias %+v %v", got, err)
	}
	if got, err := s.ResolveAlias(ctx, "doujins-legacy", "object-token", "zz99yy88"); err != nil || got.Code != links[0].Code || got.Locator != "7" {
		t.Fatalf("alias with locator %+v %v", got, err)
	}
	if _, err := s.ResolveAlias(ctx, "doujins-legacy", "tag-name", "mother x son"); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("aliases match verbatim: %v", err)
	}
	// An alias never moves: other content, another locator, or a batch that
	// disagrees with itself is ErrConflict and writes nothing.
	for _, batch := range [][]contenturl.Alias{
		{{Source: "doujins-legacy", LegacyKind: "folder", Key: "12345", ContentRef: ref("gallery", 2)}},
		{{Source: "doujins-legacy", LegacyKind: "object-token", Key: "zz99yy88", Locator: "8", ContentRef: ref("gallery", 1)}},
		{{Source: "doujins-legacy", LegacyKind: "folder", Key: "777", ContentRef: ref("gallery", 1)}, {Source: "doujins-legacy", LegacyKind: "folder", Key: "777", ContentRef: ref("gallery", 2)}},
	} {
		if err := s.PutAliases(ctx, batch...); !errors.Is(err, contenturl.ErrConflict) {
			t.Fatalf("PutAliases(%+v) = %v, want ErrConflict", batch, err)
		}
	}
	if _, err := s.ResolveAlias(ctx, "doujins-legacy", "folder", "777"); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("conflicting batch left alias 777: %v", err)
	}
	// The refusal precedes any write, so even a caller that commits anyway keeps nothing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(tx).PutAliases(ctx,
		contenturl.Alias{Source: "doujins-legacy", LegacyKind: "folder", Key: "776", ContentRef: ref("gallery", 1)},
		contenturl.Alias{Source: "doujins-legacy", LegacyKind: "folder", Key: "12345", ContentRef: ref("gallery", 2)},
	); !errors.Is(err, contenturl.ErrConflict) {
		t.Fatalf("in-tx conflict: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAlias(ctx, "doujins-legacy", "folder", "776"); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("refused batch wrote alias 776: %v", err)
	}
	err = s.PutAliases(ctx,
		contenturl.Alias{Source: "doujins-legacy", LegacyKind: "folder", Key: "778", ContentRef: ref("gallery", 1)},
		contenturl.Alias{Source: "doujins-legacy", LegacyKind: "folder", Key: "779", ContentRef: ref("gallery", 9)},
	)
	if !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("unregistered alias target: %v", err)
	}
	if _, err := s.ResolveAlias(ctx, "doujins-legacy", "folder", "778"); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("failed batch left alias 778: %v", err)
	}
	if err := s.Merge(ctx, ref("gallery", 2), ref("gallery", 1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ResolveAlias(ctx, "doujins-legacy", "tag-name", "Mother X Son"); got.Code != links[0].Code {
		t.Fatalf("alias does not follow the merge: %+v", got)
	}
	for _, bad := range []contenturl.Alias{
		{Source: "legacy", LegacyKind: "Legacy", Key: "1", ContentRef: ref("gallery", 1)},
		{Source: "legacy", LegacyKind: "legacy", Key: "", ContentRef: ref("gallery", 1)},
	} {
		if err := s.PutAliases(ctx, bad); !errors.Is(err, contenturl.ErrInvalid) {
			t.Errorf("PutAliases(%+v) = %v", bad, err)
		}
	}
}

func TestTaxonomyNodesCarryCodes(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	store, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: []string{"artist", "series"}, Languages: []string{"en", "es"}})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.CreateNodes(ctx, []taxonomy.NodeInput{
		{Kind: "artist", Slug: "artist-internal-1", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Hiroshi Tanaka"}, {Language: "ja", Kind: taxonomy.NameCanonical, Name: "田中"}}},
		{Kind: "artist", Slug: "artist-internal-2", Names: []taxonomy.Name{{Language: "es", Kind: taxonomy.NameCanonical, Name: "Señor Pérez"}}},
		{Kind: "series", Slug: "s", Names: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	refs := []contentref.ContentRef{
		contentref.New(tenant, "artist", string(nodes[0].TaxonomyID)),
		contentref.New(tenant, "artist", string(nodes[1].TaxonomyID)),
		contentref.New(tenant, "series", string(nodes[2].TaxonomyID)),
	}
	links, err := s.Links(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := links[refs[0].Key()], links[refs[1].Key()], links[refs[2].Key()]
	if a.Slug != "hiroshi-tanaka" || len(a.Slugs) != 1 || b.Slug != "senor-perez" || b.Slugs["es"] != "senor-perez" || c.Code == "" || c.Slug != "" {
		t.Fatalf("node links %+v %+v %+v", a, b, c)
	}

	if err := store.SetNames(ctx, nodes[0].TaxonomyID, []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Tanaka Hiroshi"}, {Language: "es", Kind: taxonomy.NameCanonical, Name: "Tanaka H."}}); err != nil {
		t.Fatal(err)
	}
	renamed, _ := s.Resolve(ctx, a.Code)
	if renamed.Code != a.Code || renamed.Slug != "tanaka-hiroshi" || renamed.Slugs["es"] != "tanaka-h" {
		t.Fatalf("renamed node %+v", renamed)
	}

	// The code registry joins a host's SQL transaction with taxonomy.
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WithSQLTx(tx).CreateNodes(ctx, []taxonomy.NodeInput{{Kind: "series", Slug: "rolled-back", Names: []taxonomy.Name{{Language: "en", Kind: taxonomy.NameCanonical, Name: "Gone"}}}}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()

	if _, err := store.Merge(ctx, nodes[1].TaxonomyID, nodes[0].TaxonomyID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Resolve(ctx, b.Code); err != nil || got.Code != a.Code || got.Slug != "tanaka-hiroshi" {
		t.Fatalf("merged node resolves to %+v %v", got, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.content_codes`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d codes, want 3 (the rolled-back node has none)", n)
	}
}

func TestMigrationRefusesMergeCycles(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema, db := migrateBefore0010(t, ctx, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.content_nodes (tenant_id, taxonomy_id, kind, slug, state) VALUES
 ('doujins', '%[2]s', 'tag', 'a', 'merged'), ('doujins', '%[3]s', 'tag', 'b', 'merged');
INSERT INTO %[1]s.content_edges (tenant_id, from_taxonomy_id, relation, to_taxonomy_id) VALUES
 ('doujins', '%[2]s', 'alias_of', '%[3]s'), ('doujins', '%[3]s', 'alias_of', '%[2]s')`, schema, id(1), id(2))); err != nil {
		t.Fatal(err)
	}
	if err := migrations.ApplyPostgres(ctx, db, schema); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic merges migrated: %v", err)
	}
}

// migrateBefore0010 applies the chain up to 0009 the way a host's migrate
// step does.
func migrateBefore0010(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, *sql.DB) {
	t.Helper()
	schema := pgtest.EmptySchema(t, ctx, pool)
	pgtest.EnsureExtensions(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.Postgres, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0010" {
			continue
		}
		b, err := fs.ReadFile(migrations.Postgres, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[e.Name()] = &fstest.MapFile{Data: b}
	}
	steps, err := migratekit.Load(old, ".", migratekit.RequireParentLinks())
	if err != nil {
		t.Fatal(err)
	}
	if err := migratekit.NewPostgres(db, "contentkit").WithSchema(schema).ApplyMigrations(ctx, steps); err != nil {
		t.Fatal(err)
	}
	return schema, db
}

// The migration backfills ContentKit's own records through the production
// migratekit path: nodes slugged from their names (English first), merged
// nodes redirected to their survivor, live posts.
func TestMigrationBackfillsOwnRecords(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema, db := migrateBefore0010(t, ctx, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`SET search_path TO %[1]s;
INSERT INTO %[1]s.content_nodes (tenant_id, taxonomy_id, kind, slug, state) VALUES
 ('doujins', '%[2]s', 'artist', 'a1', 'active'),
 ('doujins', '%[3]s', 'artist', 'a2', 'merged'),
 ('doujins', '%[4]s', 'tag', 't', 'active'),
 ('hentai0', '%[2]s', 'tag', 'h', 'active');
INSERT INTO %[1]s.content_node_names (tenant_id, taxonomy_id, language, kind, name) VALUES
 ('doujins', '%[2]s', 'en', 'name', 'Café Ölmüller'),
 ('doujins', '%[2]s', 'de', 'name', 'Kaffee Ölmüller'),
 ('doujins', '%[2]s', 'en', 'alias', 'Coffee'),
 ('doujins', '%[3]s', 'en', 'name', 'Dup'),
 ('hentai0', '%[2]s', 'ja', 'name', 'タグ');
INSERT INTO %[1]s.content_edges (tenant_id, from_taxonomy_id, relation, to_taxonomy_id) VALUES ('doujins', '%[3]s', 'alias_of', '%[2]s');
INSERT INTO %[1]s.content_posts (id, tenant_id, author_id, title, slug, body) VALUES
 ('%[5]s', 'doujins', 'u', 'Hello World', NULL, 'b'),
 ('%[6]s', 'doujins', 'u', 'Ignored', 'My Custom Slug', 'b');
INSERT INTO %[1]s.content_posts (id, tenant_id, author_id, title, body, deleted_at) VALUES ('%[7]s', 'doujins', 'u', 'Deleted', 'b', now());
RESET search_path`, schema, id(1), id(2), id(3), id(4), id(5), id(6))); err != nil {
		t.Fatal(err)
	}
	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}

	s := newStore(t, pool, schema, tenant)
	links, err := s.Links(ctx, []contentref.ContentRef{ref("artist", 1), ref("artist", 2), ref("tag", 3), ref("post", 4), ref("post", 5), ref("post", 6)})
	if err != nil {
		t.Fatal(err)
	}
	a1 := links[ref("artist", 1).Key()]
	if a1.Slug != "cafe-olmuller" || a1.Slugs["de"] != "kaffee-olmuller" || len(a1.Slugs) != 2 {
		t.Fatalf("artist %+v", a1)
	}
	if a2 := links[ref("artist", 2).Key()]; a2.Code != a1.Code {
		t.Fatalf("merged artist resolves to %+v, want %s", a2, a1.Code)
	}
	if tag := links[ref("tag", 3).Key()]; tag.Code == "" || tag.Slug != "" {
		t.Fatalf("nameless tag %+v", tag)
	}
	if p := links[ref("post", 4).Key()]; p.Slug != "hello-world" {
		t.Fatalf("post %+v", p)
	}
	if p := links[ref("post", 5).Key()]; p.Slug != "my-custom-slug" {
		t.Fatalf("post with slug %+v", p)
	}
	if _, ok := links[ref("post", 6).Key()]; ok || len(links) != 5 {
		t.Fatalf("deleted post registered or links missing: %+v", links)
	}
	h, err := newStore(t, pool, schema, "hentai0").Links(ctx, []contentref.ContentRef{contentref.New("hentai0", "tag", id(1))})
	if err != nil || len(h) != 1 {
		t.Fatalf("hentai0 tag %+v %v", h, err)
	}
	// New records after the backfill keep drawing codes from the same generator.
	if got := put(t, s, contenturl.Entry{ContentRef: ref("gallery", 7), Title: "new"})[0]; !codeRE.MatchString(string(got.Code)) {
		t.Fatalf("post-migration code %q", got.Code)
	}
}
