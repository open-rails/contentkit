package taxonomy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

const tenant = "doujins"

// testSchema is a disposable schema with the keyword profile and the taxonomy
// lineage, plus a host-owned version table (hv) that eligibility joins read:
// the version universe, its languages and its visibility stay host-owned.
func testSchema(t *testing.T, ctx context.Context) (*pgxpool.Pool, string) {
	t.Helper()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.hv (tenant_id text NOT NULL, content_kind text NOT NULL, content_id text NOT NULL, version_id text NOT NULL, language text NOT NULL, live boolean NOT NULL DEFAULT true, is_default boolean NOT NULL DEFAULT false, PRIMARY KEY (tenant_id, content_kind, version_id))`, schema)); err != nil {
		t.Fatal(err)
	}
	return pool, schema
}

// liveEligibility admits a document only when the host row says the version is
// live in that language; the language default has priority 0.
func liveEligibility(schema string) *search.Eligibility {
	return &search.Eligibility{SQL: fmt.Sprintf(`SELECT (NOT hv.is_default)::int AS priority FROM %s.hv hv
 WHERE hv.tenant_id=sd.tenant_id AND hv.content_kind=sd.content_kind AND hv.content_id=sd.content_id AND hv.version_id=sd.content_version_id AND hv.language=sd.language AND hv.live`, schema)}
}

func newStore(t *testing.T, pool *pgxpool.Pool, schema, tenantID string, mutate func(*Options)) *Store {
	t.Helper()
	opts := Options{Pool: pool, Schema: schema, Tenant: tenantID, Kinds: []string{"tag", "artist", "character", "series", "season", "voice_actor", "creator"}, Languages: []string{"en", "es"}, CountEligibility: liveEligibility(schema)}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type version struct {
	kind, id, version, language, title string
	live, isDefault                    bool
}

// indexVersions writes the host version rows and their keyword documents.
func indexVersions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, tenantID string, versions ...version) {
	t.Helper()
	var docs []search.KeywordDocument
	for _, v := range versions {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.hv (tenant_id, content_kind, content_id, version_id, language, live, is_default) VALUES ($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT (tenant_id, content_kind, version_id) DO UPDATE SET live=EXCLUDED.live, is_default=EXCLUDED.is_default`, schema), tenantID, v.kind, v.id, v.version, v.language, v.live, v.isDefault); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: contentref.NewVersion(tenantID, v.kind, v.id, v.version), Language: v.language}, Title: v.title})
	}
	if err := search.UpsertKeywordDocuments(ctx, pool, schema, docs); err != nil {
		t.Fatal(err)
	}
}

func setLive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, versionID string, live bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.hv SET live=$2 WHERE version_id=$1`, schema), versionID, live); err != nil {
		t.Fatal(err)
	}
}

func mustCreate(t *testing.T, ctx context.Context, s *Store, inputs ...NodeInput) []Node {
	t.Helper()
	nodes, err := s.CreateNodes(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

// tag is a tag node input; an empty id lets CreateNodes mint one.
func tag(id, slug string, names ...Name) NodeInput {
	in := NodeInput{Kind: "tag", Slug: slug, Names: names}
	if id != "" {
		in.TaxonomyID = tid(id)
	}
	return in
}

func name(lang, n string) Name  { return Name{Language: lang, Kind: NameCanonical, Name: n} }
func alias(lang, n string) Name { return Name{Language: lang, Kind: NameAlias, Name: n} }

func work(tenantID, kind, id string) contentref.ContentRef { return contentref.New(tenantID, kind, id) }

// cid is the nth deterministic content id: a canonical UUIDv7, ordered by n.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

// workLabels names the fixture works in expected strings.
var workLabels = map[string]string{
	cid(1): "g1", cid(2): "g2", cid(3): "g3", cid(4): "g4",
	cid(11): "m1", cid(18): "m8", cid(19): "m9", cid(21): "l1", cid(22): "l2",
}

// taxNames are the fixture taxonomy ids by name. tid maps a name to a
// canonical UUIDv7; ids sort in the names' order, so id orders read as before.
var taxNames = []string{
	"a", "a1", "absent", "alpha", "b", "beta", "c", "c-rin",
	"colored", "colour", "committed", "decensored", "delta", "etude", "exclusive", "fate",
	"foreign-only", "gamma", "import-a", "import-b", "jp-only", "missing", "nameless", "only-a",
	"rolled-back", "romance", "s-fate", "seller-1", "shindol", "shindol-dup", "solo", "source",
	"sql-node", "t-colored", "t-colour", "t-colour-2", "t1", "tag", "tag-a", "target",
}

var taxIDs, taxLabels = func() (map[string]TaxonomyID, map[TaxonomyID]string) {
	if !sort.StringsAreSorted(taxNames) {
		panic("taxNames must stay sorted")
	}
	ids, labels := map[string]TaxonomyID{}, map[TaxonomyID]string{}
	for i, n := range taxNames {
		id := TaxonomyID(fmt.Sprintf("01930000-0000-7000-8000-%012d", i+1))
		ids[n], labels[id] = id, n
	}
	return ids, labels
}()

// tid is the taxonomy id of a fixture name.
func tid(name string) TaxonomyID {
	id, ok := taxIDs[name]
	if !ok {
		panic("unknown taxonomy fixture " + name)
	}
	return id
}

// tname is a fixture taxonomy id's name, or the id itself.
func tname(id TaxonomyID) string {
	if n, ok := taxLabels[id]; ok {
		return n
	}
	return string(id)
}

var taxPlaceholder = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

// withIDs replaces every {{name}} in s with tid(name).
func withIDs(s string) string {
	return taxPlaceholder.ReplaceAllStringFunc(s, func(m string) string { return string(tid(m[2 : len(m)-2])) })
}

// label is a fixture work's short name, or the id itself.
func label(id string) string {
	if l, ok := workLabels[id]; ok {
		return l
	}
	return id
}

func assign(ref contentref.ContentRef, id string, relation string) Assignment {
	return Assignment{ContentRef: ref, TaxonomyID: tid(id), Relation: relation}
}

func tagIDs(tags []EffectiveTag) string {
	out := ""
	for _, t := range tags {
		if out != "" {
			out += ","
		}
		out += tname(t.TaxonomyID) + ":" + t.Relation + ":" + string(t.Scope)
	}
	return out
}

func hitIDs(hits []BrowseHit) string {
	out := ""
	for _, h := range hits {
		if out != "" {
			out += ","
		}
		out += label(h.ContentID) + "@" + h.Version() + ":" + fmt.Sprint(h.Priority)
	}
	return out
}

func countString(counts []Count) string {
	out := ""
	for _, c := range counts {
		if out != "" {
			out += ","
		}
		out += fmt.Sprintf("%s/%s=%d", c.ContentKind, c.Language, c.Count)
	}
	return out
}
