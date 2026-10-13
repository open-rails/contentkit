package contenturl_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/internal/pgtest"
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
	got, err := s.Resolve(ctx, []contenturl.Code{lower, "00000000A", lower})
	if l := got[lower]; err != nil || len(got) != 1 || l.Code != v.Code || l.Slug != "renamed" || !l.ContentRef.Equal(video) {
		t.Fatalf("Resolve %+v %v (an unknown code is absent)", got, err)
	}
	if _, err := s.Resolve(ctx, []contenturl.Code{"g4vrq3zq5"}); !errors.Is(err, contenturl.ErrInvalidCode) {
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
	got, err := other.Resolve(ctx, []contenturl.Code{mine.Code, theirs.Code})
	if _, leaked := got[mine.Code]; err != nil || leaked {
		t.Fatalf("hentai0 resolved a doujins code: %+v %v", got, err)
	}
	if l := got[theirs.Code]; l.TenantID != "hentai0" || l.Slug != "theirs" {
		t.Fatalf("own code %+v", l)
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
	ctx, _, _, s := setup(t)
	const n = 8
	var wg sync.WaitGroup
	got := make([]contenturl.Code, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			links, err := s.Put(ctx, contenturl.Entry{ContentRef: ref("video", 1), Title: fmt.Sprint("t", i)})
			if err == nil {
				got[i] = links[0].Code
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Fatalf("put %d: %q %v (first %q)", i, got[i], errs[i], got[0])
		}
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
	if got, _ := s.Resolve(ctx, []contenturl.Code{a.Code}); got[a.Code].Code != b.Code || !got[a.Code].ContentRef.Equal(ref("gallery", 2)) {
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
	all := []contenturl.Code{a.Code, b.Code, c.Code}
	got, err := s.Resolve(ctx, all)
	for _, code := range all {
		if l := got[code]; err != nil || l.Code != c.Code || l.Slug != "c" {
			t.Fatalf("%s resolves to %+v %v", code, l, err)
		}
	}
	if byKey, _ := s.Links(ctx, []contentref.ContentRef{ref("gallery", 1)}); byKey[ref("gallery", 1).Key()].Code != c.Code {
		t.Fatalf("Links of a merged ref %+v", byKey)
	}
	if err := s.Merge(ctx, ref("gallery", 9), ref("gallery", 3)); !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("unregistered merge: %v", err)
	}
}

// putAliases writes aliases as the bulk import does, committing even on a
// refusal, which writes nothing.
func putAliases(ctx context.Context, pool *pgxpool.Pool, schema string, aliases ...codes.Alias) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	err = codes.PutAliases(ctx, codes.Pgx(tx), pgx.Identifier{schema}.Sanitize(), tenant, aliases)
	if cerr := tx.Commit(ctx); err == nil {
		err = cerr
	}
	return err
}

func alias(source, kind, key, locator string, r contentref.ContentRef) codes.Alias {
	return codes.Alias{Source: source, Kind: kind, Key: key, Locator: locator, Target: codes.Key{Kind: r.ContentKind, ID: r.ContentID}}
}

func ak(kind, key string) contenturl.AliasKey {
	return contenturl.AliasKey{Source: "doujins-legacy", Kind: kind, Key: key}
}

func resolveAlias(t *testing.T, s *contenturl.Store, kind, key string) *contenturl.AliasMatch {
	t.Helper()
	got, err := s.ResolveAliases(t.Context(), []contenturl.AliasKey{ak(kind, key)})
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

func TestAliases(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	links := put(t, s, contenturl.Entry{ContentRef: ref("gallery", 1), Title: "one"}, contenturl.Entry{ContentRef: ref("gallery", 2), Title: "two"})
	aliases := []codes.Alias{
		alias("doujins-legacy", "folder", "12345", "", ref("gallery", 1)),
		alias("doujins-legacy", "folder-token", "ab12cd34", "", ref("gallery", 1)),
		alias("doujins-legacy", "object-token", "zz99yy88", "7", ref("gallery", 1)),
		alias("doujins-legacy", "tag-name", "Mother X Son", "", ref("gallery", 2)),
	}
	if err := putAliases(ctx, pool, schema, aliases...); err != nil {
		t.Fatal(err)
	}
	// An import re-run repeats its aliases: a no-op.
	if err := putAliases(ctx, pool, schema, aliases...); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	got, err := s.ResolveAliases(ctx, []contenturl.AliasKey{ak("folder", "12345"), ak("object-token", "zz99yy88"),
		ak("tag-name", "mother x son"), ak("folder", "12345")})
	if err != nil || len(got) != 4 || got[0] == nil || got[0].Code != links[0].Code || got[0].Locator != "" || got[3] == nil || got[3].Code != got[0].Code {
		t.Fatalf("alias %+v %v", got, err)
	}
	if got[1] == nil || got[1].Code != links[0].Code || got[1].Locator != "7" {
		t.Fatalf("alias with locator %+v", got[1])
	}
	if got[2] != nil {
		t.Fatalf("aliases match verbatim: %+v", got[2])
	}
	// An alias never moves: other content, another locator, or a batch that
	// disagrees with itself is ErrConflict and writes nothing.
	for _, batch := range [][]codes.Alias{
		{alias("doujins-legacy", "folder", "12345", "", ref("gallery", 2))},
		{alias("doujins-legacy", "object-token", "zz99yy88", "8", ref("gallery", 1))},
		{alias("doujins-legacy", "folder", "777", "", ref("gallery", 1)), alias("doujins-legacy", "folder", "777", "", ref("gallery", 2))},
	} {
		if err := putAliases(ctx, pool, schema, batch...); !errors.Is(err, contenturl.ErrConflict) {
			t.Fatalf("PutAliases(%+v) = %v, want ErrConflict", batch, err)
		}
	}
	if got := resolveAlias(t, s, "folder", "777"); got != nil {
		t.Fatalf("conflicting batch left alias 777: %+v", got)
	}
	// The refusal precedes any write, so even a caller that commits anyway keeps nothing.
	if err := putAliases(ctx, pool, schema,
		alias("doujins-legacy", "folder", "776", "", ref("gallery", 1)),
		alias("doujins-legacy", "folder", "12345", "", ref("gallery", 2)),
	); !errors.Is(err, contenturl.ErrConflict) {
		t.Fatalf("in-tx conflict: %v", err)
	}
	if got := resolveAlias(t, s, "folder", "776"); got != nil {
		t.Fatalf("refused batch wrote alias 776: %+v", got)
	}
	err = putAliases(ctx, pool, schema,
		alias("doujins-legacy", "folder", "778", "", ref("gallery", 1)),
		alias("doujins-legacy", "folder", "779", "", ref("gallery", 9)),
	)
	if !errors.Is(err, contenturl.ErrNotFound) {
		t.Fatalf("unregistered alias target: %v", err)
	}
	if got := resolveAlias(t, s, "folder", "778"); got != nil {
		t.Fatalf("failed batch left alias 778: %+v", got)
	}
	if err := s.Merge(ctx, ref("gallery", 2), ref("gallery", 1)); err != nil {
		t.Fatal(err)
	}
	if got := resolveAlias(t, s, "tag-name", "Mother X Son"); got == nil || got.Code != links[0].Code {
		t.Fatalf("alias does not follow the merge: %+v", got)
	}
	if _, err := s.ResolveAliases(ctx, []contenturl.AliasKey{ak("Folder", "1")}); !errors.Is(err, contenturl.ErrInvalid) {
		t.Errorf("malformed alias kind: %v", err)
	}
	for _, bad := range []codes.Alias{alias("legacy", "Legacy", "1", "", ref("gallery", 1)), alias("legacy", "legacy", "", "", ref("gallery", 1))} {
		if err := putAliases(ctx, pool, schema, bad); !errors.Is(err, contenturl.ErrInvalid) {
			t.Errorf("PutAliases(%+v) = %v", bad, err)
		}
	}
}
