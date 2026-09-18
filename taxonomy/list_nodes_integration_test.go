package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pageString renders a page as "id:name@language=count" per row, in order.
func pageString(p NodePage) string {
	out := ""
	for _, n := range p.Nodes {
		if out != "" {
			out += ","
		}
		out += fmt.Sprintf("%s:%s@%s=%d", n.TaxonomyID, n.Name, n.NameLanguage, n.Count)
	}
	return out
}

func ids(p NodePage) string {
	out := ""
	for _, n := range p.Nodes {
		if out != "" {
			out += ","
		}
		out += string(n.TaxonomyID)
	}
	return out
}

// catalogFixture is the five-node catalog every ListNodes case reads: one node
// per language situation (both languages, request language only, fallback
// language only, nameless) and per count situation (many, one, none).
func catalogFixture(t *testing.T, ctx context.Context) (*pgxpool.Pool, string, *Store) {
	t.Helper()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s,
		tag("alpha", "alpha", name("en", "Alpha"), name("es", "Alfa"), alias("en", "First Letter")),
		tag("beta", "beta", name("es", "Beta ES")),
		tag("delta", "delta", name("en", "Delta"), alias("es", "Delta Alias ES")),
		tag("etude", "etude", name("en", "Étude")),
		tag("gamma", "gamma"),
	)
	indexVersions(t, ctx, pool, schema, tenant,
		version{"gallery", "g1", "v1", "en", "Blue Ocean", true, true},
		version{"gallery", "g2", "v2", "en", "Green Field", true, true},
		version{"video", "m1", "w1", "en", "Red Sky", true, true},
	)
	if err := s.Assign(ctx, []Assignment{
		assign(work(tenant, "gallery", "g1"), "alpha", ""),
		assign(work(tenant, "gallery", "g2"), "alpha", ""),
		assign(work(tenant, "video", "m1"), "alpha", ""),
		assign(work(tenant, "gallery", "g1"), "etude", ""),
		assign(work(tenant, "gallery", "g1"), "delta", ""),
	}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	// Distinct timestamps: created descends alpha..gamma, updated ascends, so
	// the two orders are exact reverses and cannot pass by accident.
	for i, id := range []string{"alpha", "beta", "delta", "etude", "gamma"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_nodes SET created_at = now() - make_interval(days => $2), updated_at = now() - make_interval(days => $3) WHERE tenant_id=$1 AND taxonomy_id=$4`, schema), tenant, i, 4-i, id); err != nil {
			t.Fatal(err)
		}
	}
	return pool, schema, s
}

// The request language resolves the display name, falling back to the store's
// configured languages and then to any; strict and required change what a node
// without a name in that language gets.
func TestListNodes_Language(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)

	page, err := s.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alpha@en=3,beta:Beta ES@es=0,delta:Delta@en=1,etude:Étude@en=1,gamma:@=0" {
		t.Fatalf("en fallback: %s %v", pageString(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "es", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alfa@es=0,beta:Beta ES@es=0,delta:Delta@en=0,etude:Étude@en=0,gamma:@=0" {
		t.Fatalf("es fallback: %s %v", pageString(page), err)
	}
	// Counts are per document language: the corpus is indexed in en only.
	if page.Nodes[0].Count != 0 {
		t.Fatalf("es counts must be their own: %s", pageString(page))
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", LanguageMode: LanguageStrict, Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alpha@en=3,delta:Delta@en=1,etude:Étude@en=1,beta:@=0,gamma:@=0" {
		t.Fatalf("strict: %s %v", pageString(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", LanguageMode: LanguageRequired, Sort: SortName})
	if err != nil || ids(page) != "alpha,delta,etude" {
		t.Fatalf("required: %s %v", pageString(page), err)
	}
	// An unset language is the store's first configured language.
	def, err := s.ListNodes(ctx, ListOptions{Sort: SortName})
	if err != nil || pageString(def) != pageString(mustList(t, ctx, s, ListOptions{Language: "en", Sort: SortName})) {
		t.Fatalf("default language: %s %v", pageString(def), err)
	}
}

// The A-Z index and the name+alias search match the normalized form, so
// diacritics and case do not matter and a wildcard stays literal.
func TestListNodes_PrefixAndQuery(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)

	for _, tc := range []struct{ prefix, want string }{
		{"e", "etude"},   // Étude normalizes to etude
		{"É", "etude"},   // and so does the accented request
		{"al", "alpha"},  // over the resolved display name
		{"beta", "beta"}, // matched through the fallback language
		{"%", ""},        // a wildcard is literal, not "everything"
		{"nothing", ""},
	} {
		page, err := s.ListNodes(ctx, ListOptions{Language: "en", NamePrefix: tc.prefix, Sort: SortName})
		if err != nil || ids(page) != tc.want {
			t.Fatalf("prefix %q: %s %v, want %q", tc.prefix, ids(page), err, tc.want)
		}
	}
	for _, tc := range []struct{ query, want string }{
		{"lph", "alpha"},          // substring of the canonical name
		{"first letter", "alpha"}, // an alias in the request language
		{"alias es", "delta"},     // an alias in another language
		{"ALFA", "alpha"},         // case- and language-insensitive
		{"etude", "etude"},
		{"%", ""},
		{"_", ""},
	} {
		page, err := s.ListNodes(ctx, ListOptions{Language: "en", Query: tc.query, Sort: SortName})
		if err != nil || ids(page) != tc.want {
			t.Fatalf("query %q: %s %v, want %q", tc.query, ids(page), err, tc.want)
		}
	}
}

// Counts come from content_node_counts for the request language, scoped to one
// content kind or summed across them; MinCount is the hide-empty predicate.
func TestListNodes_Counts(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)

	page, err := s.ListNodes(ctx, ListOptions{Language: "en", ContentKind: "gallery", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alpha@en=2,beta:Beta ES@es=0,delta:Delta@en=1,etude:Étude@en=1,gamma:@=0" {
		t.Fatalf("gallery counts: %s %v", pageString(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", ContentKind: "video", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alpha@en=1,beta:Beta ES@es=0,delta:Delta@en=0,etude:Étude@en=0,gamma:@=0" {
		t.Fatalf("video counts: %s %v", pageString(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", MinCount: 1, ContentKind: "gallery", Sort: SortName})
	if err != nil || ids(page) != "alpha,delta,etude" {
		t.Fatalf("hide empty: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", MinCount: 2, Sort: SortName})
	if err != nil || ids(page) != "alpha" {
		t.Fatalf("min count 2 over every kind: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", MinCount: 1, ContentKind: "video", Sort: SortName})
	if err != nil || ids(page) != "alpha" {
		t.Fatalf("hide empty per kind: %s %v", ids(page), err)
	}
}

// Every order is total: taxonomy_id breaks every tie.
func TestListNodes_Sort(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)

	for _, tc := range []struct {
		sort Sort
		want string
	}{
		{SortID, "alpha,beta,delta,etude,gamma"},
		{SortName, "alpha,beta,delta,etude,gamma"}, // nameless gamma last
		{SortCount, "alpha,delta,etude,beta,gamma"},
		{SortCreated, "alpha,beta,delta,etude,gamma"},
		{SortUpdated, "gamma,etude,delta,beta,alpha"},
	} {
		page, err := s.ListNodes(ctx, ListOptions{Language: "en", Sort: tc.sort})
		if err != nil || ids(page) != tc.want {
			t.Fatalf("sort %q: %s %v, want %q", tc.sort, ids(page), err, tc.want)
		}
	}
	// Nameless last is a property of the order, not of the fixture's ids.
	page, err := s.ListNodes(ctx, ListOptions{Language: "en", LanguageMode: LanguageStrict, Sort: SortName})
	if err != nil || ids(page) != "alpha,delta,etude,beta,gamma" {
		t.Fatalf("strict name order: %s %v", ids(page), err)
	}
}

// Offset paging carries the unpaged total; cursor paging keeps its keyset
// contract and refuses to mix with either an order or an offset.
func TestListNodes_Paging(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)

	page, err := s.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName, Limit: 2})
	if err != nil || ids(page) != "alpha,beta" || page.Total != 5 || page.NextCursor != "" {
		t.Fatalf("offset page 1: %s total=%d cursor=%q %v", ids(page), page.Total, page.NextCursor, err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName, Limit: 2, Offset: 2})
	if err != nil || ids(page) != "delta,etude" || page.Total != 5 {
		t.Fatalf("offset page 2: %s total=%d %v", ids(page), page.Total, err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName, Limit: 2, Offset: 4})
	if err != nil || ids(page) != "gamma" || page.Total != 5 {
		t.Fatalf("offset page 3: %s total=%d %v", ids(page), page.Total, err)
	}
	// The total is the filtered total, not the tenant's node count.
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", MinCount: 1, Sort: SortCount, Limit: 1})
	if err != nil || ids(page) != "alpha" || page.Total != 3 {
		t.Fatalf("filtered total: %s total=%d %v", ids(page), page.Total, err)
	}

	// Cursor paging: unchanged contract, and it does not pay for a count.
	first, err := s.ListNodes(ctx, ListOptions{Limit: 2})
	if err != nil || ids(first) != "alpha,beta" || first.NextCursor != "beta" || first.Total != 5 {
		t.Fatalf("cursor page 1: %s cursor=%q total=%d %v", ids(first), first.NextCursor, first.Total, err)
	}
	second, err := s.ListNodes(ctx, ListOptions{Limit: 2, Cursor: first.NextCursor})
	if err != nil || ids(second) != "delta,etude" || second.NextCursor != "etude" || second.Total != 0 {
		t.Fatalf("cursor page 2: %s total=%d %v", ids(second), second.Total, err)
	}
	last, err := s.ListNodes(ctx, ListOptions{Limit: 2, Cursor: second.NextCursor})
	if err != nil || ids(last) != "gamma" || last.NextCursor != "" {
		t.Fatalf("cursor page 3: %s cursor=%q %v", ids(last), last.NextCursor, err)
	}
}

// States, IDs and the option validations.
func TestListNodes_StatesIDsAndValidation(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)
	deleted := StateDeleted
	if _, err := s.UpdateNode(ctx, "gamma", NodeUpdate{State: &deleted}); err != nil {
		t.Fatal(err)
	}

	page, err := s.ListNodes(ctx, ListOptions{Sort: SortName})
	if err != nil || ids(page) != "alpha,beta,delta,etude" {
		t.Fatalf("default state: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{States: []State{StateDeleted}})
	if err != nil || ids(page) != "gamma" {
		t.Fatalf("deleted only: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{States: []State{StateActive, StateDeleted}, Sort: SortName})
	if err != nil || ids(page) != "alpha,beta,delta,etude,gamma" {
		t.Fatalf("active and deleted: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{IDs: []TaxonomyID{"alpha", "etude", "absent"}, Sort: SortName})
	if err != nil || ids(page) != "alpha,etude" {
		t.Fatalf("by ids: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{IDs: []TaxonomyID{"alpha"}, Kind: "series"})
	if err != nil || len(page.Nodes) != 0 {
		t.Fatalf("ids stay filtered: %s %v", ids(page), err)
	}

	for name, opts := range map[string]ListOptions{
		"unknown relation":      {Related: "alpha", Relation: "likes"},
		"relation without node": {Relation: RelationMemberOf},
		"bad related id":        {Related: "has space"},
		"unknown sort":          {Sort: "popularity"},
		"unknown state":         {States: []State{"archived"}},
		"unknown language mode": {LanguageMode: "guess"},
		"unregistered kind":     {Kind: "studio"},
		"bad language":          {Language: "not a language"},
		"bad id":                {IDs: []TaxonomyID{"has space"}},
		"cursor with sort":      {Cursor: "alpha", Sort: SortName},
		"cursor with offset":    {Cursor: "alpha", Offset: 10},
	} {
		if _, err := s.ListNodes(ctx, opts); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// Related is the generic "members of this node" index page: the characters of
// a series, the artists of a circle.
func TestListNodes_Related(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)
	mustCreate(t, ctx, s, NodeInput{TaxonomyID: "fate", Kind: "series", Slug: "fate", Names: []Name{name("en", "Fate")}})
	if err := s.AddEdges(ctx, []Edge{
		{From: "alpha", Relation: RelationMemberOf, To: "fate"},
		{From: "delta", Relation: RelationMemberOf, To: "fate"},
		{From: "beta", Relation: RelationSynonym, To: "fate"},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListNodes(ctx, ListOptions{Language: "en", Related: "fate", Sort: SortName})
	if err != nil || ids(page) != "alpha,beta,delta" || page.Total != 3 {
		t.Fatalf("any relation: %s total=%d %v", ids(page), page.Total, err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", Related: "fate", Relation: RelationMemberOf, Sort: SortName})
	if err != nil || ids(page) != "alpha,delta" || page.Total != 2 {
		t.Fatalf("member_of: %s total=%d %v", ids(page), page.Total, err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Language: "en", Related: "fate", Relation: RelationMemberOf, Kind: "tag", MinCount: 2, Sort: SortName})
	if err != nil || ids(page) != "alpha" {
		t.Fatalf("combined with the other filters: %s %v", ids(page), err)
	}
	page, err = s.ListNodes(ctx, ListOptions{Related: "gamma"})
	if err != nil || len(page.Nodes) != 0 {
		t.Fatalf("no members: %s %v", ids(page), err)
	}
}

// Every join carries tenant_id: a node of another tenant with the same id
// lends neither its name nor its count.
func TestListNodes_TenantScoping(t *testing.T) {
	ctx := context.Background()
	pool, schema, a := catalogFixture(t, ctx)
	const other = "hentai0"
	b := newStore(t, pool, schema, other, nil)
	mustCreate(t, ctx, b, tag("alpha", "alpha", name("en", "Other Alpha"), alias("en", "first letter")), tag("solo", "solo", name("en", "Solo")))
	indexVersions(t, ctx, pool, schema, other,
		version{"video", "m9", "w9", "en", "Their Work", true, true},
		version{"video", "m8", "w8", "en", "Their Other Work", true, true},
	)
	if err := b.Assign(ctx, []Assignment{
		assign(work(other, "video", "m9"), "alpha", ""),
		assign(work(other, "video", "m8"), "alpha", ""),
		assign(work(other, "video", "m9"), "solo", ""),
	}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}

	page, err := a.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Alpha@en=3,beta:Beta ES@es=0,delta:Delta@en=1,etude:Étude@en=1,gamma:@=0" {
		t.Fatalf("A's page after B's writes: %s %v", pageString(page), err)
	}
	page, err = b.ListNodes(ctx, ListOptions{Language: "en", Sort: SortName})
	if err != nil || pageString(page) != "alpha:Other Alpha@en=2,solo:Solo@en=1" {
		t.Fatalf("B's page: %s %v", pageString(page), err)
	}
	// B's alias must not pull A's node into B's result set, or the reverse.
	page, err = b.ListNodes(ctx, ListOptions{Language: "en", Query: "first letter", Sort: SortName})
	if err != nil || ids(page) != "alpha" || page.Total != 1 {
		t.Fatalf("B's query: %s total=%d %v", ids(page), page.Total, err)
	}
	page, err = a.ListNodes(ctx, ListOptions{Language: "en", Query: "solo", Sort: SortName})
	if err != nil || len(page.Nodes) != 0 || page.Total != 0 {
		t.Fatalf("A must not see B's node: %s %v", ids(page), err)
	}
}

// The admin route exposes the same options and answers the same page.
func TestListNodes_HTTP(t *testing.T) {
	ctx := context.Background()
	_, _, s := catalogFixture(t, ctx)
	srv := httptest.NewServer(Handler(s))
	t.Cleanup(srv.Close)

	get := func(query string) NodePage {
		t.Helper()
		res, err := http.Get(srv.URL + "/nodes" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s -> %d %s", query, res.StatusCode, body)
		}
		var page NodePage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("GET %s -> %s: %v", query, body, err)
		}
		return page
	}

	page := get("?language=en&sort=count&content_kind=gallery&min_count=1&limit=2")
	if ids(page) != "alpha,delta" || page.Total != 3 || page.Nodes[0].Name != "Alpha" || page.Nodes[0].Count != 2 {
		t.Fatalf("index page: %s total=%d %+v", ids(page), page.Total, page.Nodes)
	}
	if page := get("?language=en&sort=count&content_kind=gallery&min_count=1&limit=2&offset=2"); ids(page) != "etude" || page.Total != 3 {
		t.Fatalf("index page 2: %s total=%d", ids(page), page.Total)
	}
	if page := get("?language=en&name_prefix=al"); ids(page) != "alpha" {
		t.Fatalf("letter index: %s", ids(page))
	}
	if page := get("?language=en&q=first+letter"); ids(page) != "alpha" {
		t.Fatalf("search: %s", ids(page))
	}
	if page := get("?language=en&language_mode=required&sort=name"); ids(page) != "alpha,delta,etude" {
		t.Fatalf("required language: %s", ids(page))
	}
	if page := get("?id=alpha&id=gamma&sort=name"); ids(page) != "alpha,gamma" {
		t.Fatalf("by id: %s", ids(page))
	}
	if page := get("?state=active&state=deleted&sort=name"); ids(page) != "alpha,beta,delta,etude,gamma" {
		t.Fatalf("by state: %s", ids(page))
	}

	res, err := http.Get(srv.URL + "/nodes?sort=popularity")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown sort -> %d, want 400", res.StatusCode)
	}
}

func mustList(t *testing.T, ctx context.Context, s *Store, opts ListOptions) NodePage {
	t.Helper()
	page, err := s.ListNodes(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
