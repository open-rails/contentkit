package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestListNodesHostFilterIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	first := newStore(t, pool, schema, "first", nil)
	second := newStore(t, pool, schema, "second", nil)
	for _, store := range []*Store{first, second} {
		_, err := store.CreateNodes(ctx, []NodeInput{
			{TaxonomyID: tid("a"), Kind: "artist", Slug: "a", Names: []Name{{Language: "en", Kind: NameCanonical, Name: "Alpha"}}},
			{TaxonomyID: tid("b"), Kind: "artist", Slug: "b", Names: []Name{{Language: "en", Kind: NameCanonical, Name: "Beta"}}},
			{TaxonomyID: tid("c"), Kind: "artist", Slug: "c", Names: []Name{{Language: "en", Kind: NameCanonical, Name: "Gamma"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, withIDs(fmt.Sprintf(`CREATE TABLE %s.host_meta(tenant_id text,taxonomy_id text,category text);
 INSERT INTO %[1]s.host_meta VALUES ('first','{{a}}','circle'),('first','{{b}}','individual'),('first','{{c}}','circle'),('second','{{b}}','circle')`, schema))); err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{Kind: "artist", Sort: SortName, Limit: 1, FilterSQL: fmt.Sprintf(`EXISTS(SELECT 1 FROM %s.host_meta m WHERE m.tenant_id=n.tenant_id AND m.taxonomy_id=n.taxonomy_id AND m.category=@host_category)`, schema), FilterArgs: map[string]any{"host_category": "circle"}}
	for offset, want := range []TaxonomyID{tid("a"), tid("c")} {
		opts.Offset = offset
		page, err := first.ListNodes(ctx, opts)
		if err != nil || page.Total != 2 || len(page.Nodes) != 1 || page.Nodes[0].TaxonomyID != want {
			t.Fatalf("offset %d: %+v %v", offset, page, err)
		}
	}
	opts.Offset = 2
	page, err := first.ListNodes(ctx, opts)
	if err != nil || page.Total != 2 || len(page.Nodes) != 0 {
		t.Fatalf("empty page total: %+v %v", page, err)
	}
	opts.Offset = 0
	page, err = second.ListNodes(ctx, opts)
	if err != nil || page.Total != 1 || len(page.Nodes) != 1 || page.Nodes[0].TaxonomyID != tid("b") {
		t.Fatalf("tenant sidecar scope: %+v %v", page, err)
	}
	opts.FilterArgs["host_category"] = "circle' OR true --"
	page, err = first.ListNodes(ctx, opts)
	if err != nil || page.Total != 0 || len(page.Nodes) != 0 {
		t.Fatalf("bound value became SQL: %+v %v", page, err)
	}
	opts.FilterSQL = "false OR true"
	opts.FilterArgs = nil
	page, err = first.ListNodes(ctx, opts)
	if err != nil || page.Total != 3 {
		t.Fatalf("host OR escaped tenant predicate: %+v %v", page, err)
	}
}

func TestListNodesRejectsReservedHostArgs(t *testing.T) {
	s := &Store{tenant: "first", languages: []string{"en"}}
	for _, name := range []string{"tenant", "language", "fallback", "states", "kind", "slug", "related", "relation", "ids", "content_kind", "prefix", "query", "min_count", "cursor", "limit", "offset", "", " bad", "a-b"} {
		_, err := s.compileList(ListOptions{FilterArgs: map[string]any{name: "override"}})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("FilterArgs[%q] accepted: %v", name, err)
		}
	}
}
