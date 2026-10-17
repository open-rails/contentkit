package groups

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/kinds"
)

// creatableFixture makes tags, artists, series and characters creatable and
// configures a filter tag and a required tag.
func creatableFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	cfg := config()
	for _, k := range []string{"tag", "artist", "series", "character"} {
		gk := cfg.GroupKinds[k]
		gk.Creatable = true
		cfg.GroupKinds[k] = gk
	}
	cfg.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"gallery"}, Domains: []kinds.Domain{{Name: "main",
		RequireEntities: map[string][]kinds.GroupRef{"gallery": {{Kind: "tag", Slug: "yaoi"}}},
		Filters:         []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "furry"}}}}}}
	s, err := New(Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

func (f *fixture) groupsOf(content int, kind string) map[string]string {
	f.t.Helper()
	q := pgx.Identifier{f.schema}.Sanitize()
	rows, err := f.pool.Query(f.ctx, `SELECT g.slug, g.id FROM `+q+`.content_memberships m JOIN `+q+`.content_groups g ON g.tenant_id=m.tenant_id AND g.id=m.group_id
 WHERE m.tenant_id=$1 AND m.content_id=$2 AND g.kind=$3 AND m.version_id IS NULL`, tenant, cid(content), kind)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for rows.Next() {
		var slug, id string
		if err := rows.Scan(&slug, &id); err != nil {
			f.t.Fatal(err)
		}
		out[slug] = id
	}
	return out
}

func (f *fixture) countKind(kind string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+pgx.Identifier{f.schema}.Sanitize()+`.content_groups WHERE tenant_id=$1 AND kind=$2`, tenant, kind).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestContentSetCreatesNamedEntities(t *testing.T) {
	f := creatableFixture(t)
	f.content("gallery", 1, live("en"))
	cafe := f.create(GroupInput{Kind: "tag", Slug: "cafe-au-lait", Names: []Name{canonical("en", "Café Au Lait"), alias("es", "Café con leche")}},
		GroupInput{Kind: "voice_actor", Slug: "rie", Names: []Name{canonical("en", "Rie Kugimiya")}})
	set := ContentGroups{ContentID: cid(1), Create: []NewEntity{
		{Kind: "tag", Name: "cafe-au lait"}, {Kind: "tag", Name: "CAFE CON LECHE"}, {Kind: "tag", Name: "Brand New"},
		{Kind: "character", Name: "Saber"}, {Kind: "series", Name: "Fate Stay Night"}, {Kind: "voice_actor", Name: "rie kugimiya"},
	}}
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{set}); err != nil {
		t.Fatal(err)
	}
	tags := f.groupsOf(1, "tag")
	if len(tags) != 2 || tags["cafe-au-lait"] != string(cafe[0].ID) || tags["brand-new"] == "" {
		t.Fatalf("tags: existing names and aliases link across languages, a new name is created once: %v", tags)
	}
	series, characters := f.groupsOf(1, "series"), f.groupsOf(1, "character")
	if len(series) != 1 || len(characters) != 1 || characters["saber"] == "" {
		t.Fatalf("a new character sits under the new series named alongside it: %v %v", series, characters)
	}
	var scope string
	if err := f.pool.QueryRow(f.ctx, `SELECT scope_id FROM `+pgx.Identifier{f.schema}.Sanitize()+`.content_groups WHERE id=$1`, characters["saber"]).Scan(&scope); err != nil || scope != series["fate-stay-night"] {
		t.Fatalf("character scope %q, want %q (%v)", scope, series["fate-stay-night"], err)
	}
	if va := f.groupsOf(1, "voice_actor"); va["rie"] != string(cafe[1].ID) {
		t.Fatalf("an existing entity of a non-creatable kind still links: %v", va)
	}

	// Repeating the save creates nothing and keeps the same links.
	before := map[string]int{"tag": f.countKind("tag"), "series": f.countKind("series"), "character": f.countKind("character")}
	set.Memberships = nil
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{set}); err != nil {
		t.Fatal(err)
	}
	for kind, n := range before {
		if got := f.countKind(kind); got != n {
			t.Errorf("%s count %d after a repeat, want %d", kind, got, n)
		}
	}
	if got := f.groupsOf(1, "tag"); len(got) != 2 || got["brand-new"] != tags["brand-new"] {
		t.Errorf("repeat links %v, want %v", got, tags)
	}
}

func TestContentSetRefusesUncreatableNames(t *testing.T) {
	f := creatableFixture(t)
	f.content("gallery", 1, live("en"))
	tagsBefore := f.countKind("tag")
	for name, ne := range map[string]NewEntity{
		"non-creatable kind":          {Kind: "voice_actor", Name: "Nobody Yet"},
		"configured filter entity":    {Kind: "tag", Name: "Furry"},
		"required entity":             {Kind: "tag", Name: "Yaoi"},
		"character without a series":  {Kind: "character", Name: "Rin"},
		"kind the content won't join": {Kind: "channel", Name: "Mine"},
		"empty name":                  {Kind: "tag", Name: "  "},
	} {
		if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), Create: []NewEntity{ne}}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if f.countKind("tag") != tagsBefore || f.countKind("character") != 0 {
		t.Error("a refused save created entities")
	}
	f.create(GroupInput{Kind: "tag", Slug: "twin-a", Names: []Name{canonical("en", "Twin")}}, GroupInput{Kind: "tag", Slug: "twin-b", Names: []Name{alias("en", "Twin")}})
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), Create: []NewEntity{{Kind: "tag", Name: "twin"}}}}); !errors.Is(err, ErrConflict) {
		t.Errorf("an ambiguous name: %v, want ErrConflict", err)
	}
}

func TestConcurrentContentSetsCreateOneEntity(t *testing.T) {
	f := creatableFixture(t)
	const n = 8
	for i := range n {
		f.content("gallery", i+1, live("en"))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(i + 1), Create: []NewEntity{{Kind: "tag", Name: "Racer"}}}})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := f.countKind("tag"); got != 1 {
		t.Fatalf("%d tags after concurrent saves of one new name, want 1", got)
	}
	ids := []string{}
	for i := range n {
		for _, id := range f.groupsOf(i+1, "tag") {
			ids = append(ids, id)
		}
	}
	if len(ids) != n || len(slices.Compact(slices.Sorted(slices.Values(ids)))) != 1 {
		t.Fatalf("every content links the one tag: %v", ids)
	}
}

func TestContentSetScopesNewCharacters(t *testing.T) {
	f := creatableFixture(t)
	for i := range 4 {
		f.content("gallery", i+1, live("en"))
	}
	g := f.create(GroupInput{Kind: "series", Slug: "fate", Names: []Name{canonical("en", "Fate")}}, GroupInput{Kind: "series", Slug: "zero", Names: []Name{canonical("en", "Zero")}})
	fate, zero := g[0], g[1]
	saber := f.create(GroupInput{Kind: "character", Slug: "saber", ScopeID: fate.ID, Names: []Name{canonical("en", "Saber")}})[0]
	set := func(content int, series []GroupID, names ...string) error {
		ms := []Membership{}
		for _, id := range series {
			ms = append(ms, Membership{GroupID: id})
		}
		create := []NewEntity{}
		for _, n := range names {
			create = append(create, NewEntity{Kind: "character", Name: n})
		}
		return f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(content), Memberships: ms, Create: create}})
	}

	// The gallery's one series scopes the match: Fate's Saber links.
	if err := set(1, []GroupID{fate.ID}, "saber"); err != nil {
		t.Fatal(err)
	}
	if got := f.groupsOf(1, "character"); got["saber"] != string(saber.ID) {
		t.Fatalf("Fate's existing Saber: %v", got)
	}
	// The same name in another series is another character.
	if err := set(2, []GroupID{zero.ID}, "Saber"); err != nil {
		t.Fatal(err)
	}
	other := f.groupsOf(2, "character")
	if len(other) != 1 || other["saber"] == "" || other["saber"] == string(saber.ID) || f.countKind("character") != 2 {
		t.Fatalf("Zero's Saber must be a new character: %v (%d characters)", other, f.countKind("character"))
	}
	// Zero or several series never guess a scope, even for an existing name.
	if err := set(3, nil, "Saber"); !errors.Is(err, ErrInvalid) {
		t.Errorf("no series: %v, want ErrInvalid", err)
	}
	if err := set(4, []GroupID{fate.ID, zero.ID}, "Saber"); !errors.Is(err, ErrInvalid) {
		t.Errorf("two series: %v, want ErrInvalid", err)
	}
	if f.countKind("character") != 2 {
		t.Errorf("refused saves created characters: %d", f.countKind("character"))
	}
}
