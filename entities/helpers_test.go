package entities

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

const tenant = "doujins"

func config() kinds.Config {
	return kinds.Config{
		Languages: []string{"en", "es"},
		Kinds: map[string]kinds.Kind{
			"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"},
				Entities: []string{"tag", "artist", "series", "character", "voice_actor", "rail"}},
			"video": {Route: "watch", Presentation: "player", Versions: kinds.VersionRules{Languages: "many"}, Entities: []string{"tag", "channel"},
				Locked: &kinds.Locked{Show: []string{kinds.ShowEntities}}},
		},
		EntityKinds: map[string]kinds.EntityKind{
			"tag": {Route: "tags", VersionLevel: true},
			"artist": {Route: "artists", Roles: []string{"publisher"},
				Attributes: map[string]kinds.Attribute{"artist_type": {Type: "enum", Values: []string{"artist", "circle", "company"}}}},
			"series":      {Route: "series", Ordered: true, Parent: "series"},
			"character":   {Route: "characters", ScopedBy: "series", Attributes: map[string]kinds.Attribute{"external_link": {Type: "string"}}},
			"voice_actor": {Route: "voice-actors", VersionLevel: true, Avatar: true},
			"rail":        {Ordered: true, Windows: true},
			"channel":     {Route: "c", Owner: "user", AuthScope: true, Entitled: true},
		},
	}
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	schema string
	s      *Store
	// changed records every Options.ContentChanged call.
	changed [][]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: pgtest.Schema(t, ctx, pool)}
	s, err := New(Options{Pool: pool, Schema: f.schema, Tenant: tenant, Kinds: config(),
		ContentChanged: func(_ context.Context, _ Querier, ids []string) error {
			f.changed = append(f.changed, slices.Clone(ids))
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

// cid is the nth deterministic content id; vid(n, i) its ith version.
func cid(n int) string    { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }
func vid(n, i int) string { return fmt.Sprintf("01920000-0000-7000-9000-%010d%02d", n, i) }

// version is one content version with its facet: live from live (nil: not
// live).
type version struct {
	languages []string
	live      *time.Time
}

var past = time.Now().Add(-time.Hour)

func live(langs ...string) version { return version{languages: langs, live: &past} }

// content writes content n of kind with its versions and their facets, as the
// catalog lane will.
func (f *fixture) content(kind string, n int, versions ...version) {
	f.t.Helper()
	q := pgx.Identifier{f.schema}.Sanitize()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+q+`.content_items (tenant_id, id, kind, state) VALUES ($1, $2, $3, 'published')`, tenant, cid(n), kind); err != nil {
		f.t.Fatal(err)
	}
	for i, v := range versions {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+q+`.content_versions (tenant_id, id, content_id, position, languages) VALUES ($1, $2, $3, $4, $5)`,
			tenant, vid(n, i), cid(n), i, v.languages); err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+q+`.content_facets (tenant_id, content_id, version_id, kind, live_from, languages, access_level, published_at)
 VALUES ($1, $2, $3, $4, $5, $6, 'public', $5)`, tenant, cid(n), vid(n, i), kind, v.live, v.languages); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) create(inputs ...EntityInput) []Entity {
	f.t.Helper()
	out, err := f.s.CreateEntities(f.ctx, inputs)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) add(ms ...Membership) {
	f.t.Helper()
	if err := f.s.AddMemberships(f.ctx, ms); err != nil {
		f.t.Fatal(err)
	}
}

// facetEntities is each version's stored content_facets.entity_ids, by version.
func (f *fixture) facetEntities() map[string][]string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT version_id, entity_ids FROM `+pgx.Identifier{f.schema}.Sanitize()+`.content_facets`)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string][]string{}
	for rows.Next() {
		var v string
		var ids []string
		if err := rows.Scan(&v, &ids); err != nil {
			f.t.Fatal(err)
		}
		out[v] = ids
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, strings.ReplaceAll(sql, "{s}", pgx.Identifier{f.schema}.Sanitize()), args...); err != nil {
		f.t.Fatal(err)
	}
}

func canonical(lang, name string) Name { return Name{Language: lang, Kind: NameCanonical, Name: name} }
func alias(lang, name string) Name     { return Name{Language: lang, Kind: NameAlias, Name: name} }

func ids(gs ...Entity) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g.ID)
	}
	slices.Sort(out)
	return out
}

func ptr[T any](v T) *T { return &v }
