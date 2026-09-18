package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"testing"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/worker"
)

func TestNodesNamesEdgesAndMergeIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)

	nodes := mustCreate(t, ctx, s,
		tag("t-colored", "colored", name("en", "Colored"), name("es", "A color"), alias("en", "Full Color")),
		NodeInput{Kind: "tag", Slug: "romance", Names: []Name{name("en", "Romance")}, SourceRevision: 7},
		NodeInput{TaxonomyID: "s-fate", Kind: "series", Slug: "fate", Names: []Name{name("en", "Fate"), name("ja", "フェイト")}},
		NodeInput{TaxonomyID: "c-rin", Kind: "character", Slug: "rin", Names: []Name{name("en", "Rin")}},
	)
	if len(nodes) != 4 || nodes[0].TaxonomyID != "t-colored" || nodes[1].TaxonomyID == "" || nodes[1].SourceRevision != 7 || nodes[0].State != StateActive {
		t.Fatalf("created %+v", nodes)
	}
	romance := nodes[1].TaxonomyID
	for name, in := range map[string]NodeInput{
		"duplicate slug": tag("", "colored"),
		"duplicate id":   tag("t-colored", "other"),
	} {
		if _, err := s.CreateNodes(ctx, []NodeInput{in}); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]NodeInput{
		"unregistered kind":   {Kind: "studio", Slug: "x"},
		"empty slug":          {Kind: "tag", Slug: " "},
		"two canonical names": {Kind: "tag", Slug: "x", Names: []Name{name("en", "A"), name("en", "B")}},
		"bad language":        {Kind: "tag", Slug: "x", Names: []Name{name("", "A")}},
	} {
		if _, err := s.CreateNodes(ctx, []NodeInput{in}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A creation queues one typeahead document per configured language.
	var dirty int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_kind='tag' AND content_id='t-colored' AND NOT is_deleted`, schema), tenant).Scan(&dirty); err != nil || dirty != 2 {
		t.Fatalf("dirty rows %d: %v", dirty, err)
	}

	d, err := s.Node(ctx, "t-colored")
	if err != nil || len(d.Names) != 3 || d.Names[0].Name != "Colored" || d.Names[0].Normalized != "colored" || d.Names[1].Name != "Full Color" || d.Names[2].Language != "es" {
		t.Fatalf("node: %+v %v", d, err)
	}
	// A new canonical name demotes the old one to an alias; equal normalized forms collapse.
	if err := s.AddNames(ctx, "t-colored", []Name{name("en", "Coloured"), alias("en", "COLORED"), alias("es", "Coloreado")}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Node(ctx, "t-colored")
	got := ""
	for _, n := range d.Names {
		got += fmt.Sprintf("%s:%s:%s,", n.Language, n.Kind, n.Name)
	}
	if got != "en:name:Coloured,en:alias:COLORED,en:alias:Full Color,es:name:A color,es:alias:Coloreado," {
		t.Fatalf("names after rename: %s", got)
	}
	if err := s.RemoveNames(ctx, "t-colored", []Name{alias("en", "full color"), alias("en", "unknown")}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNames(ctx, "s-fate", []Name{name("en", "Fate/stay night")}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Node(ctx, "t-colored"); len(d.Names) != 4 {
		t.Fatalf("names after remove: %+v", d.Names)
	}
	if d, _ = s.Node(ctx, "s-fate"); len(d.Names) != 1 || d.Names[0].Name != "Fate/stay night" {
		t.Fatalf("names after set: %+v", d.Names)
	}
	if _, err := s.Node(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node: %v", err)
	}
	if err := s.AddNames(ctx, "missing", []Name{name("en", "x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("names of a missing node: %v", err)
	}

	// Listing: kind filter and keyset paging.
	page, err := s.ListNodes(ctx, ListOptions{Kind: "tag", Limit: 1})
	if err != nil || len(page.Nodes) != 1 || page.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	page2, err := s.ListNodes(ctx, ListOptions{Kind: "tag", Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(page2.Nodes) != 1 || page2.NextCursor != "" || page2.Nodes[0].TaxonomyID == page.Nodes[0].TaxonomyID {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	if page, err = s.ListNodes(ctx, ListOptions{Slug: "fate"}); err != nil || len(page.Nodes) != 1 || page.Nodes[0].Kind != "series" {
		t.Fatalf("by slug: %+v %v", page, err)
	}
	if _, err := s.ListNodes(ctx, ListOptions{Kind: "studio"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unregistered kind list: %v", err)
	}

	// Edges.
	if err := s.AddEdges(ctx, []Edge{{From: "c-rin", Relation: RelationMemberOf, To: "s-fate"}, {From: "c-rin", Relation: RelationMemberOf, To: "s-fate", SourceRevision: 2}}); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Edge{
		"self loop":    {From: "c-rin", Relation: RelationSynonym, To: "c-rin"},
		"bad relation": {From: "c-rin", Relation: "likes", To: "s-fate"},
	} {
		if err := s.AddEdges(ctx, []Edge{e}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := s.AddEdges(ctx, []Edge{{From: "c-rin", Relation: RelationMemberOf, To: "missing"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edge to missing: %v", err)
	}
	edges, err := s.Edges(ctx, []TaxonomyID{"s-fate"})
	if err != nil || len(edges) != 1 || edges[0].From != "c-rin" || edges[0].SourceRevision != 2 {
		t.Fatalf("edges: %+v %v", edges, err)
	}

	// Merge: colour folds into colored.
	mustCreate(t, ctx, s, tag("t-colour", "colour", name("en", "Colour"), alias("en", "Coloured")))
	g1, g2 := work(tenant, "gallery", "g1"), work(tenant, "gallery", "g2")
	if err := s.Assign(ctx, []Assignment{assign(g1, "t-colored", ""), assign(g1, "t-colour", ""), assign(g2, "t-colour", "tag"), assign(g2, string(romance), "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEdges(ctx, []Edge{{From: "t-colour", Relation: RelationSynonym, To: romance}, {From: "t-colored", Relation: RelationSynonym, To: "t-colour"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Merge(ctx, "t-colour", "t-colour"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self merge: %v", err)
	}
	if _, err := s.Merge(ctx, "t-colour", "s-fate"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-kind merge: %v", err)
	}
	if _, err := s.Merge(ctx, "t-colour", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge into missing: %v", err)
	}
	report, err := s.Merge(ctx, "t-colour", "t-colored")
	if err != nil || report.AssignmentsMoved != 1 || report.AssignmentsMerged != 1 || report.NamesMoved != 1 || report.EdgesRewritten != 1 {
		t.Fatalf("merge report: %+v %v", report, err)
	}
	merged, _ := s.Node(ctx, "t-colour")
	if merged.State != StateMerged || len(merged.Names) != 0 || len(merged.Edges) != 1 || merged.Edges[0].Relation != RelationAliasOf || merged.Edges[0].To != "t-colored" {
		t.Fatalf("merged node: %+v", merged)
	}
	target, _ := s.Node(ctx, "t-colored")
	if len(target.Names) != 5 || target.Names[2].Name != "Colour" || len(target.Edges) != 2 {
		t.Fatalf("merge target: %+v", target)
	}
	tags, err := s.EffectiveTags(ctx, []contentref.ContentRef{g1, g2})
	if err != nil || tagIDs(tags[g1.Key()]) != "t-colored:tag:content" || tagIDs(tags[g2.Key()]) != fmt.Sprintf("%s:tag:content,t-colored:tag:content", romance) {
		t.Fatalf("effective after merge: %v %v", tags, err)
	}
	if err := s.Assign(ctx, []Assignment{assign(g1, "t-colour", "")}, AssignOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assign to merged: %v", err)
	}
	if _, err := s.UpdateNode(ctx, "t-colour", NodeUpdate{Slug: ptr("x")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("update merged: %v", err)
	}
	// The merged slug is released; the merged node's documents are queued for deletion.
	mustCreate(t, ctx, s, tag("t-colour-2", "colour"))
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_kind='tag' AND content_id='t-colour' AND is_deleted`, schema), tenant).Scan(&dirty); err != nil || dirty != 2 {
		t.Fatalf("merged dirty rows %d: %v", dirty, err)
	}
	// Delete and restore.
	deleted := StateDeleted
	if n, err := s.UpdateNode(ctx, "t-colored", NodeUpdate{State: &deleted}); err != nil || n.State != StateDeleted {
		t.Fatalf("delete: %+v %v", n, err)
	}
	if tags, _ = s.EffectiveTags(ctx, []contentref.ContentRef{g1}); len(tags[g1.Key()]) != 0 {
		t.Fatalf("deleted node still effective: %v", tags)
	}
	if page, _ = s.ListNodes(ctx, ListOptions{Kind: "tag", State: StateDeleted}); len(page.Nodes) != 1 {
		t.Fatalf("deleted listing: %+v", page)
	}
	active := StateActive
	if _, err := s.UpdateNode(ctx, "t-colored", NodeUpdate{State: &active, Slug: ptr("full-color")}); err != nil {
		t.Fatal(err)
	}
	if tags, _ = s.EffectiveTags(ctx, []contentref.ContentRef{g1}); tagIDs(tags[g1.Key()]) != "t-colored:tag:content" {
		t.Fatalf("restored: %v", tags)
	}
}

func ptr[T any](v T) *T { return &v }

// The eligibility case: a Spanish request for "colored" must not be satisfied
// by an English colored edition plus a Spanish original of the same work.
func TestEffectiveTagsAndVersionEligibilityIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s, tag("colored", "colored", name("en", "Colored")), tag("romance", "romance", name("en", "Romance")), tag("decensored", "decensored"))
	indexVersions(t, ctx, pool, schema, tenant,
		version{"gallery", "g1", "v1", "en", "Blue Ocean", true, true},
		version{"gallery", "g1", "v2", "en", "Blue Ocean", true, false},
		version{"gallery", "g1", "v3", "es", "Océano Azul", true, true},
		version{"gallery", "g2", "v4", "es", "Océano Rojo", true, true},
	)
	g1 := work(tenant, "gallery", "g1")
	g1v2, g1v3 := g1.WithVersion("v2"), g1.WithVersion("v3")
	g2 := work(tenant, "gallery", "g2")
	if err := s.Assign(ctx, []Assignment{assign(g1, "romance", ""), assign(g1v2, "colored", ""), assign(g2, "romance", ""), assign(g2, "colored", ""), {ContentRef: g1v3, TaxonomyID: "decensored", State: AssignmentProposed}}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	// A work row and a version row of the same node dedupe to the work row.
	if err := s.Assign(ctx, []Assignment{assign(g1v2, "romance", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	tags, err := s.EffectiveTags(ctx, []contentref.ContentRef{g1, g1.WithVersion("v1"), g1v2, g1v3, g2, work(tenant, "gallery", "none")})
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[contentref.ContentRef]string{
		g1: "romance:tag:content", g1.WithVersion("v1"): "romance:tag:content", g1v2: "colored:tag:version,romance:tag:content",
		g1v3: "romance:tag:content", g2: "colored:tag:content,romance:tag:content", work(tenant, "gallery", "none"): "",
	} {
		if got := tagIDs(tags[ref.Key()]); got != want {
			t.Fatalf("%s effective %q want %q", ref, got, want)
		}
	}
	stored, err := s.Assignments(ctx, []contentref.ContentRef{g1, g1v2, g1v3})
	if err != nil || len(stored[g1.Key()]) != 1 || len(stored[g1v2.Key()]) != 2 || stored[g1v3.Key()][0].State != AssignmentProposed {
		t.Fatalf("stored: %+v %v", stored, err)
	}

	browse := func(language string, ids ...string) string {
		t.Helper()
		var require []TaxonomyID
		for _, id := range ids {
			require = append(require, TaxonomyID(id))
		}
		page, err := s.Browse(ctx, BrowseOptions{ContentKind: "gallery", Language: language, RequireAll: require, Eligibility: liveEligibility(schema)})
		if err != nil {
			t.Fatal(err)
		}
		return hitIDs(page.Hits)
	}
	for _, c := range []struct {
		lang, want string
		ids        []string
	}{
		{"en", "g1@v2:1", []string{"colored"}},
		{"es", "g2@v4:0", []string{"colored"}},
		{"es", "g1@v3:0,g2@v4:0", []string{"romance"}},
		{"en", "g1@v1:0", []string{"romance"}},
		{"en", "g1@v2:1", []string{"romance", "colored"}},
		{"es", "g2@v4:0", []string{"romance", "colored"}},
		{"es", "", []string{"decensored"}},
	} {
		if got := browse(c.lang, c.ids...); got != c.want {
			t.Fatalf("browse %s %v = %q want %q", c.lang, c.ids, got, c.want)
		}
	}
	// The host join decides per version: hiding g2's Spanish edition hides it.
	setLive(t, ctx, pool, schema, "v4", false)
	if got := browse("es", "colored"); got != "" {
		t.Fatalf("hidden version browsed: %q", got)
	}
	setLive(t, ctx, pool, schema, "v4", true)
	if _, err := s.Browse(ctx, BrowseOptions{ContentKind: "gallery", Language: "es"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("browse without nodes: %v", err)
	}
	if _, err := s.Browse(ctx, BrowseOptions{ContentKind: "tag", Language: "es", RequireAll: []TaxonomyID{"colored"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("browse a taxonomy kind: %v", err)
	}
	page, err := s.Browse(ctx, BrowseOptions{ContentKind: "gallery", Language: "es", RequireAll: []TaxonomyID{"romance"}, Limit: 1})
	if err != nil || hitIDs(page.Hits) != "g1@v3:0" || !page.HasMore {
		t.Fatalf("paged browse: %+v %v", page, err)
	}

	// The same filter inside keyword search: the requested trait and the
	// requested language hold on one document.
	client, err := contentkit.NewClient(contentkit.ClientConfig{Pool: pool, Schema: schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	filter, args, err := RequireAll(schema, []TaxonomyID{"colored"})
	if err != nil {
		t.Fatal(err)
	}
	for lang, want := range map[string]string{"es": "g2@v4", "en": "g1@v2"} {
		res, err := client.Search(ctx, "océano", contentkit.SearchOptions{Language: lang, ContentKinds: []string{"gallery"}, FilterSQL: filter, FilterArgs: args, Eligibility: liveEligibility(schema)})
		if lang == "en" {
			res, err = client.Search(ctx, "blue ocean", contentkit.SearchOptions{Language: lang, ContentKinds: []string{"gallery"}, FilterSQL: filter, FilterArgs: args, Eligibility: liveEligibility(schema)})
		}
		if err != nil || len(res.Hits) != 1 || res.Hits[0].ContentID+"@"+res.Hits[0].Version() != want {
			t.Fatalf("search %s: %+v %v", lang, res.Hits, err)
		}
	}
	if _, _, err := RequireAll(schema, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RequireAll without ids: %v", err)
	}
}

func TestPerLanguageCountsSuppressionAndRebuildIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s, tag("colored", "colored"), tag("romance", "romance"), NodeInput{TaxonomyID: "a1", Kind: "artist", Slug: "a1"})
	indexVersions(t, ctx, pool, schema, tenant,
		version{"gallery", "g1", "v1", "en", "Blue Ocean", true, true},
		version{"gallery", "g1", "v2", "en", "Blue Ocean", true, false},
		version{"gallery", "g1", "v3", "es", "Océano Azul", true, true},
		version{"gallery", "g2", "v4", "es", "Océano Rojo", true, true},
		version{"gallery", "g3", "v5", "es", "Océano Verde", true, true},
		version{"gallery", "g4", "v6", "en", "Draft", false, true},
	)
	g1, g2, g3, g4 := work(tenant, "gallery", "g1"), work(tenant, "gallery", "g2"), work(tenant, "gallery", "g3"), work(tenant, "gallery", "g4")
	if err := s.Assign(ctx, []Assignment{assign(g1, "romance", ""), assign(g1.WithVersion("v2"), "colored", ""), assign(g2, "romance", ""), assign(g2, "colored", ""), assign(g4, "romance", ""), assign(g1, "a1", "artist"), assign(g1, "a1", "publisher")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	counts := func(ids ...string) string {
		t.Helper()
		var list []TaxonomyID
		for _, id := range ids {
			list = append(list, TaxonomyID(id))
		}
		got, err := s.Counts(ctx, list)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, id := range ids {
			out += id + "{" + countString(got[TaxonomyID(id)]) + "}"
		}
		return out
	}
	// A work counts once per language across its editions; a trait counts its
	// edition's language only; a hidden version never counts; two relations
	// to one node count the work once.
	if got := counts("colored", "romance", "a1"); got != "colored{gallery/en=1,gallery/es=1}romance{gallery/en=1,gallery/es=2}a1{gallery/en=1,gallery/es=1}" {
		t.Fatalf("counts: %s", got)
	}
	// Suppressed writes leave counts stale; the rebuild converges them.
	if err := s.Assign(ctx, []Assignment{assign(g3, "romance", "")}, AssignOptions{SuppressCounts: true}); err != nil {
		t.Fatal(err)
	}
	if got := counts("romance"); got != "romance{gallery/en=1,gallery/es=2}" {
		t.Fatalf("suppressed write recounted: %s", got)
	}
	if n, err := s.RebuildCounts(ctx); err != nil || n != 3 {
		t.Fatalf("rebuild %d: %v", n, err)
	}
	if got := counts("romance"); got != "romance{gallery/en=1,gallery/es=3}" {
		t.Fatalf("after rebuild: %s", got)
	}
	// Host-side visibility changes are recounted per content.
	setLive(t, ctx, pool, schema, "v5", false)
	setLive(t, ctx, pool, schema, "v6", true)
	if err := s.RecountContent(ctx, []contentref.ContentRef{g3, g4}); err != nil {
		t.Fatal(err)
	}
	if got := counts("romance"); got != "romance{gallery/en=2,gallery/es=2}" {
		t.Fatalf("after recount: %s", got)
	}
	// Removing the trait edition's assignment drops its language count.
	if err := s.Unassign(ctx, []Assignment{assign(g1.WithVersion("v2"), "colored", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := counts("colored"); got != "colored{gallery/es=1}" {
		t.Fatalf("after unassign: %s", got)
	}
	// Deleting a node removes its counts; restoring recomputes them.
	deleted, active := StateDeleted, StateActive
	if _, err := s.UpdateNode(ctx, "romance", NodeUpdate{State: &deleted}); err != nil {
		t.Fatal(err)
	}
	if got := counts("romance"); got != "romance{}" {
		t.Fatalf("deleted counts: %s", got)
	}
	if _, err := s.UpdateNode(ctx, "romance", NodeUpdate{State: &active}); err != nil {
		t.Fatal(err)
	}
	if got := counts("romance"); got != "romance{gallery/en=2,gallery/es=2}" {
		t.Fatalf("restored counts: %s", got)
	}
	d, err := s.Node(ctx, "romance")
	if err != nil || countString(d.Counts) != "gallery/en=2,gallery/es=2" {
		t.Fatalf("node counts: %+v %v", d.Counts, err)
	}
	// Without a count eligibility every document counts.
	open := newStore(t, pool, schema, tenant, func(o *Options) { o.CountEligibility = nil })
	if _, err := open.RebuildCounts(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts("romance"); got != "romance{gallery/en=2,gallery/es=3}" {
		t.Fatalf("open counts: %s", got)
	}
}

func TestTypeaheadDocumentsIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s,
		NodeInput{TaxonomyID: "shindol", Kind: "artist", Slug: "shindol", Names: []Name{name("en", "Shindol"), name("ja", "シンドル"), alias("en", "ShindoL")}},
		NodeInput{TaxonomyID: "jp-only", Kind: "artist", Slug: "jp-only", Names: []Name{name("ja", "作者")}},
		NodeInput{TaxonomyID: "nameless", Kind: "artist", Slug: "nameless"},
		tag("colored", "colored", name("en", "Colored"), name("es", "A color"), alias("es", "Coloreado")),
	)
	sync := func() {
		t.Helper()
		opts := worker.Options{Pool: pool, Schema: schema, Tenant: tenant, SupportedLanguages: []string{"en", "es"}, ContentKinds: s.Kinds(), ListContent: s.Lister(nil), BuildKeywordDocuments: s.Builder(nil)}
		for i := 0; i < 3; i++ {
			if err := worker.SyncOnce(ctx, opts); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync()
	client, err := contentkit.NewClient(contentkit.ClientConfig{Pool: pool, Schema: schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	ahead := func(lang, q string, kinds ...string) string {
		t.Helper()
		hits, err := client.Typeahead(ctx, q, contentkit.TypeaheadOptions{Language: lang, ContentKinds: kinds})
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, h := range hits {
			out += h.ContentKind + "/" + h.ContentID + ","
		}
		return out
	}
	for _, c := range []struct {
		lang, q, want string
		kinds         []string
	}{
		{"en", "shindol", "artist/shindol,", []string{"artist"}},
		{"en", "シンドル", "artist/shindol,", []string{"artist"}},        // other-language name as alias
		{"es", "shindol", "artist/shindol,", []string{"artist"}},     // English canonical name titles the Spanish document
		{"es", "coloreado", "tag/colored,", []string{"tag"}},         // same-language alias
		{"en", "a color", "tag/colored,", []string{"tag", "artist"}}, // Spanish canonical name is an English alias
		{"en", "作者", "artist/jp-only,", []string{"artist"}},          // only a Japanese name: it titles every document
		{"en", "nameless", "", []string{"artist"}},                   // no names, no document
	} {
		if got := ahead(c.lang, c.q, c.kinds...); got != c.want {
			t.Fatalf("typeahead %s %q = %q want %q", c.lang, c.q, got, c.want)
		}
	}
	// Renames and merges flow through the dirty queue.
	if err := s.AddNames(ctx, "shindol", []Name{name("en", "Shindo-L")}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, ctx, s, NodeInput{TaxonomyID: "shindol-dup", Kind: "artist", Slug: "shindol-dup", Names: []Name{name("en", "Shindol Dup")}})
	sync()
	if got := ahead("en", "shindol dup", "artist"); got != "artist/shindol-dup," {
		t.Fatalf("before merge: %q", got)
	}
	if _, err := s.Merge(ctx, "shindol-dup", "shindol"); err != nil {
		t.Fatal(err)
	}
	sync()
	if got := ahead("en", "shindol dup", "artist"); got != "artist/shindol," {
		t.Fatalf("after merge: %q", got)
	}
	deleted := StateDeleted
	if _, err := s.UpdateNode(ctx, "colored", NodeUpdate{State: &deleted}); err != nil {
		t.Fatal(err)
	}
	sync()
	if got := ahead("es", "coloreado", "tag"); got != "" {
		t.Fatalf("deleted node still suggested: %q", got)
	}
	// Backfill from an empty queue and index rebuilds every active node.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.content_search_documents; DELETE FROM %s.content_search_backfill; DELETE FROM %s.content_search_dirty`, schema, schema, schema)); err != nil {
		t.Fatal(err)
	}
	sync()
	var docs int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.content_search_documents WHERE tenant_id=$1`, schema), tenant).Scan(&docs); err != nil || docs != 4 {
		t.Fatalf("backfilled documents %d: %v", docs, err)
	}
	if got := ahead("en", "shindo", "artist"); got != "artist/shindol," {
		t.Fatalf("after backfill: %q", got)
	}
	// A builder never serves another tenant or an unregistered kind.
	if _, err := s.BuildKeywordDocuments(ctx, "hentai0", "artist", "en", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign tenant build: %v", err)
	}
	if _, err := s.Builder(nil)(ctx, tenant, "gallery", "en", nil); err == nil {
		t.Fatal("unregistered kind without next builder")
	}
}

func TestTenantIsolationIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	const other = "hentai0"
	a := newStore(t, pool, schema, tenant, nil)
	b := newStore(t, pool, schema, other, nil)
	// The same id exists in both tenants with different meaning.
	mustCreate(t, ctx, a, tag("t1", "colored", name("en", "Colored")), tag("only-a", "romance"))
	mustCreate(t, ctx, b, tag("t1", "uncensored", name("en", "Uncensored")))
	indexVersions(t, ctx, pool, schema, tenant, version{"gallery", "g1", "v1", "en", "Blue Ocean", true, true})
	indexVersions(t, ctx, pool, schema, other, version{"video", "m1", "w1", "en", "Blue Ocean", true, true})
	if err := a.Assign(ctx, []Assignment{assign(work(tenant, "gallery", "g1"), "t1", ""), assign(work(tenant, "gallery", "g1"), "only-a", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Assign(ctx, []Assignment{assign(work(other, "video", "m1"), "t1", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Node(ctx, "only-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B read A's node: %v", err)
	}
	if n, err := b.Node(ctx, "t1"); err != nil || n.Slug != "uncensored" || len(n.Names) != 1 || n.Names[0].Name != "Uncensored" || countString(n.Counts) != "video/en=1" {
		t.Fatalf("B's own t1: %+v %v", n, err)
	}
	if n, err := a.Node(ctx, "t1"); err != nil || countString(n.Counts) != "gallery/en=1" {
		t.Fatalf("A's t1 after B's write: %+v %v", n, err)
	}
	if page, err := b.ListNodes(ctx, ListOptions{}); err != nil || len(page.Nodes) != 1 {
		t.Fatalf("B listing: %+v %v", page, err)
	}
	if err := b.AddEdges(ctx, []Edge{{From: "t1", Relation: RelationSynonym, To: "only-a"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B linked A's node: %v", err)
	}
	if err := b.Assign(ctx, []Assignment{assign(work(other, "video", "m1"), "only-a", "")}, AssignOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B assigned A's node: %v", err)
	}
	if err := b.Assign(ctx, []Assignment{assign(work(tenant, "gallery", "g1"), "t1", "")}, AssignOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("B assigned A's content: %v", err)
	}
	if _, err := b.EffectiveTags(ctx, []contentref.ContentRef{work(tenant, "gallery", "g1")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("B read A's content: %v", err)
	}
	if _, err := b.Merge(ctx, "t1", "only-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B merged into A's node: %v", err)
	}
	if err := b.AddNames(ctx, "only-a", []Name{name("en", "x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B named A's node: %v", err)
	}
	page, err := b.Browse(ctx, BrowseOptions{ContentKind: "gallery", Language: "en", RequireAll: []TaxonomyID{"t1"}, Eligibility: liveEligibility(schema)})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("B browsed A's content: %+v %v", page, err)
	}
	if page, err = b.Browse(ctx, BrowseOptions{ContentKind: "video", Language: "en", RequireAll: []TaxonomyID{"t1"}, Eligibility: liveEligibility(schema)}); err != nil || hitIDs(page.Hits) != "m1@w1:0" {
		t.Fatalf("B's own browse: %+v %v", page, err)
	}
	if docs, err := b.BuildKeywordDocuments(ctx, other, "tag", "en", []contentref.ContentRef{work(other, "tag", "only-a"), work(other, "tag", "t1")}); err != nil || len(docs) != 1 || docs[0].Title != "Uncensored" {
		t.Fatalf("B built A's document: %+v %v", docs, err)
	}
	if n, err := b.RebuildCounts(ctx); err != nil || n != 1 {
		t.Fatalf("B rebuilt %d nodes: %v", n, err)
	}
	if n, err := a.Node(ctx, "only-a"); err != nil || countString(n.Counts) != "gallery/en=1" {
		t.Fatalf("A's counts after B's rebuild: %+v %v", n, err)
	}
}

// Marketplace: a seller is a creator node assigned to a listing under the
// seller relation; entitlement gating stays in the host join.
func TestMarketplaceListingFixtureIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	const market = "market"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.entitlements (subject text NOT NULL, content_id text NOT NULL, PRIMARY KEY (subject, content_id));
 INSERT INTO %s.entitlements VALUES ('buyer', 'l2')`, schema, schema)); err != nil {
		t.Fatal(err)
	}
	// Published listings are public; an unpublished one is visible to entitled subjects.
	entitled := func(subject string) *contentkit.Eligibility {
		return &contentkit.Eligibility{SQL: fmt.Sprintf(`SELECT (NOT hv.is_default)::int AS priority FROM %s.hv hv
 WHERE hv.tenant_id=sd.tenant_id AND hv.content_kind=sd.content_kind AND hv.content_id=sd.content_id AND hv.version_id=sd.content_version_id AND hv.language=sd.language
   AND (hv.live OR EXISTS (SELECT 1 FROM %s.entitlements e WHERE e.subject=@subject AND e.content_id=sd.content_id))`, schema, schema), Args: map[string]any{"subject": subject}}
	}
	s := newStore(t, pool, schema, market, func(o *Options) { o.Kinds = []string{"creator", "tag"}; o.Languages = []string{"en"} })
	mustCreate(t, ctx, s,
		NodeInput{TaxonomyID: "seller-1", Kind: "creator", Slug: "acme", Names: []Name{name("en", "Acme Studio")}},
		NodeInput{TaxonomyID: "exclusive", Kind: "tag", Slug: "exclusive", Names: []Name{name("en", "Exclusive")}},
	)
	indexVersions(t, ctx, pool, schema, market,
		version{"listing", "l1", "l1v1", "en", "Brush Pack", true, true},
		version{"listing", "l1", "l1v2", "en", "Brush Pack Pro", true, false},
		version{"listing", "l2", "l2v1", "en", "Preview Pack", false, true},
	)
	l1, l2 := work(market, "listing", "l1"), work(market, "listing", "l2")
	if err := s.Assign(ctx, []Assignment{assign(l1, "seller-1", "seller"), assign(l2, "seller-1", "seller"), assign(l1.WithVersion("l1v2"), "exclusive", "")}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	tags, err := s.EffectiveTags(ctx, []contentref.ContentRef{l1, l1.WithVersion("l1v2")})
	if err != nil || tagIDs(tags[l1.Key()]) != "seller-1:seller:content" || tagIDs(tags[l1.WithVersion("l1v2").Key()]) != "exclusive:tag:version,seller-1:seller:content" {
		t.Fatalf("listing tags: %v %v", tags, err)
	}
	for subject, want := range map[string]string{"anon": "l1@l1v1:0", "buyer": "l1@l1v1:0,l2@l2v1:0"} {
		page, err := s.Browse(ctx, BrowseOptions{ContentKind: "listing", Language: "en", RequireAll: []TaxonomyID{"seller-1"}, Eligibility: entitled(subject)})
		if err != nil || hitIDs(page.Hits) != want {
			t.Fatalf("%s browse: %+v %v", subject, page, err)
		}
	}
	page, err := s.Browse(ctx, BrowseOptions{ContentKind: "listing", Language: "en", RequireAll: []TaxonomyID{"seller-1", "exclusive"}, Eligibility: entitled("buyer")})
	if err != nil || hitIDs(page.Hits) != "l1@l1v2:1" {
		t.Fatalf("exclusive browse: %+v %v", page, err)
	}
	// Public counts see published listings only.
	if n, err := s.Node(ctx, "seller-1"); err != nil || countString(n.Counts) != "listing/en=1" {
		t.Fatalf("seller counts: %+v %v", n.Counts, err)
	}
	// A listing is a host content kind, never a taxonomy kind.
	if _, err := New(Options{Pool: pool, Schema: schema, Tenant: market, Kinds: []string{"Listing!"}, Languages: []string{"en"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad kind accepted: %v", err)
	}
}
