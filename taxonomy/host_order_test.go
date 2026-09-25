package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestHostOrderPreservesPagedDirectory(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	store := newStore(t, pool, schema, "first", nil)
	_, err := store.CreateNodes(ctx, []NodeInput{
		{TaxonomyID: tid("a"), Kind: "artist", Slug: "a"},
		{TaxonomyID: tid("b"), Kind: "artist", Slug: "b"},
		{TaxonomyID: tid("c"), Kind: "artist", Slug: "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, withIDs(fmt.Sprintf(`CREATE TABLE %s.host_order(tenant_id text,taxonomy_id text,language text,sort_key text);
INSERT INTO %[1]s.host_order VALUES('first','{{a}}','ja','かな'),('first','{{b}}','ja','あい'),('first','{{c}}','ja','あい'),('other','{{a}}','ja','ああ');
UPDATE %[1]s.content_nodes SET created_at=CASE taxonomy_id WHEN '{{a}}' THEN '2020-01-03'::timestamptz WHEN '{{b}}' THEN '2020-01-01'::timestamptz ELSE '2020-01-02'::timestamptz END WHERE tenant_id='first'`, schema)))
	if err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{Kind: "artist", Limit: 1, OrderSQL: fmt.Sprintf(`(SELECT m.sort_key FROM %s.host_order m WHERE m.tenant_id=n.tenant_id AND m.taxonomy_id=n.taxonomy_id AND m.language=@host_language) ASC NULLS LAST`, schema), FilterArgs: map[string]any{"host_language": "ja"}}
	for offset, want := range []TaxonomyID{tid("b"), tid("c"), tid("a")} {
		opts.Offset = offset
		page, err := store.ListNodes(ctx, opts)
		if err != nil || page.Total != 3 || len(page.Nodes) != 1 || page.Nodes[0].TaxonomyID != want || page.NextCursor != "" {
			t.Fatalf("page%d: %+v %v", offset, page, err)
		}
	}
	opts.Offset = 3
	page, err := store.ListNodes(ctx, opts)
	if err != nil || len(page.Nodes) != 0 || page.Total != 3 {
		t.Fatalf("past end: %+v %v", page, err)
	}
	// A bound value cannot become part of the ORDER BY clause.
	opts.Offset = 0
	opts.FilterArgs["host_language"] = "ja' OR true --"
	page, err = store.ListNodes(ctx, opts)
	if err != nil || len(page.Nodes) != 1 || page.Nodes[0].TaxonomyID != tid("a") {
		t.Fatalf("bound ordering value: %+v %v", page, err)
	}
	page, err = store.ListNodes(ctx, ListOptions{Kind: "artist", Sort: SortOldest, Limit: 3})
	if err != nil || len(page.Nodes) != 3 || page.Nodes[0].TaxonomyID != tid("b") || page.Nodes[1].TaxonomyID != tid("c") || page.Nodes[2].TaxonomyID != tid("a") {
		t.Fatalf("oldest: %+v %v", page, err)
	}
}

func TestHostOrderRejectsIncompatiblePaging(t *testing.T) {
	s := &Store{tenant: "first", languages: []string{"en"}}
	for _, opts := range []ListOptions{{OrderSQL: "n.created_at", Sort: SortName}, {OrderSQL: "n.created_at", Cursor: "a"}} {
		if _, err := s.compileList(opts); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ambiguous order accepted: %+v %v", opts, err)
		}
	}
}
