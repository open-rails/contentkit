package locationpolicy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/content"
)

func TestMatch(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.45")
	location := content.VisitorLocation{Country: "US", Subdivision: "US-TX"}
	for _, tc := range []struct {
		name     string
		entry    content.LocationEntry
		location content.VisitorLocation
		want     string
	}{
		{"country", content.LocationEntry{Country: "US"}, location, "country"},
		{"other country", content.LocationEntry{Country: "DE"}, location, ""},
		{"subdivision", content.LocationEntry{Subdivision: "US-TX"}, location, "subdivision"},
		{"other subdivision", content.LocationEntry{Subdivision: "US-CA"}, location, ""},
		{"single address", content.LocationEntry{Network: "203.0.113.45/32"}, location, "network"},
		{"CIDR", content.LocationEntry{Network: "203.0.113.0/24"}, location, "network"},
		{"other network", content.LocationEntry{Network: "203.0.114.0/24"}, location, ""},
		{"unknown country", content.LocationEntry{Country: "US"}, content.VisitorLocation{}, ""},
		{"unknown subdivision", content.LocationEntry{Subdivision: "US-TX"}, content.VisitorLocation{}, ""},
		{"network with lookup miss", content.LocationEntry{Network: "203.0.113.0/24"}, content.VisitorLocation{}, "network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.entry, ip, tc.location); got != tc.want {
				t.Fatalf("matched %q, want %q", got, tc.want)
			}
		})
	}
	if Match(content.LocationEntry{Network: "203.0.113.0/24"}, netip.Addr{}, location) != "" {
		t.Fatal("invalid IP matched")
	}
	if Match(content.LocationEntry{Network: "203.0.113.0/24"}, netip.MustParseAddr("::ffff:203.0.113.45"), location) != "network" {
		t.Fatal("mapped IPv4 did not match")
	}
}

func TestBlock(t *testing.T) {
	b := Block{ContentIDs: []string{"item"}, EntityIDs: []string{"tag"}, Ruled: true}
	if b.Allows("item", nil, "") || b.Allows("other", []string{"tag"}, "") || !b.Allows("other", []string{"uncovered"}, "") {
		t.Fatal("content/tag boundary failed")
	}
	if b.Allows("other", nil, "tag") || !b.Allows("other", nil, "channel") {
		t.Fatal("owner boundary failed")
	}
	if (Block{Site: true}).Allows("other", nil, "") {
		t.Fatal("site boundary failed")
	}
	predicate, args := b.SQL("f", "loc")
	if predicate == "true" || len(args) != 2 {
		t.Fatalf("missing SQL boundary: %q %v", predicate, args)
	}
	// An engine request reuses its one read, without sharing state with the next
	// request, tenant or domain. No querier is needed for this already-read block.
	ctx := WithBlock(context.Background(), "tenant", "main", b)
	if Varies(ctx) {
		t.Fatal("unread content rules made the answer vary")
	}
	got, err := Resolve(ctx, nil, "schema", "tenant", "main", "203.0.113.45")
	if err != nil || got.Allows("item", nil, "") {
		t.Fatalf("request boundary was lost: %+v %v", got, err)
	}
	if !Varies(ctx) {
		t.Fatal("a consulted rule did not make the answer vary")
	}
}

func TestGeoLookupAndReload(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "City.mmdb")
	fixture, err := os.ReadFile("testdata/GeoIP2-City-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := t.Context()
	l := g.Lookup(ctx, netip.MustParseAddr("81.2.69.142"))
	if l.Country != "GB" || l.Subdivision != "GB-ENG" || l.CountryName != "United Kingdom" || l.SubdivisionName != "England" {
		t.Fatalf("unexpected fixture location: %+v", l)
	}
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "192.0.2.1", "::1"} {
		if got := g.Lookup(ctx, netip.MustParseAddr(ip)); got.Country != "" {
			t.Fatalf("%s should be unknown: %+v", ip, got)
		}
	}
	previous := g.reader
	changed := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	g.checked = time.Now().Add(-2 * time.Minute)
	if got := g.Lookup(ctx, netip.MustParseAddr("81.2.69.142")); got.Country != "GB" || g.reader == previous {
		t.Fatal("database was not reopened")
	}
	previous = g.reader
	replacement := filepath.Join(filepath.Dir(path), "replacement.mmdb")
	if err := os.WriteFile(replacement, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	changed = changed.Add(time.Minute)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	g.checked = time.Now().Add(-2 * time.Minute)
	if got := g.Lookup(ctx, netip.MustParseAddr("81.2.69.142")); got.Country != "GB" || g.reader != previous {
		t.Fatal("failed replacement lost working database")
	}
	empty, err := Open("", log)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if got := empty.Lookup(ctx, netip.MustParseAddr("81.2.69.142")); got.Country != "" {
		t.Fatal("unconfigured database returned a location")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing"), log); err == nil {
		t.Fatal("configured missing database accepted")
	}
}

func TestGeoConcurrentLookupAndClose(t *testing.T) {
	g, err := Open("testdata/GeoIP2-City-Test.mmdb", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ip := netip.MustParseAddr("81.2.69.142")
	done := make(chan content.VisitorLocation, 1)
	g.mu.RLock()
	go func() { done <- g.Lookup(t.Context(), ip) }()
	select {
	case got := <-done:
		g.mu.RUnlock()
		if got.Country != "GB" {
			t.Fatalf("concurrent lookup: %+v", got)
		}
	case <-time.After(5 * time.Second):
		g.mu.RUnlock()
		<-done
		t.Fatal("lookup waited for another reader")
	}
	// Race a due reload and active lookups with closing the mmap.
	g.checked = time.Now().Add(-2 * time.Minute)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { g.Lookup(t.Context(), ip) })
	}
	wg.Go(func() {
		if err := g.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	wg.Wait()
	if got := g.Lookup(t.Context(), ip); got.Country != "" {
		t.Fatalf("closed database returned a location: %+v", got)
	}
}

func TestResolveFromPostgres(t *testing.T) {
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL is required for the database rule-query test")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("ck_locations_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
	migration, err := os.ReadFile("../migrations/postgres/0070_location_rules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SET LOCAL search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `CREATE TABLE content_items (tenant_id text,id text,PRIMARY KEY (tenant_id,id)); CREATE TABLE content_entities (tenant_id text,id text,PRIMARY KEY (tenant_id,id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	rules := quoted + ".content_location_rules"
	entries := quoted + ".content_location_rule_entries"
	for _, r := range []struct {
		id, tenant, domain, target    string
		enabled                       bool
		country, subdivision, network string
		contents, tags                []string
	}{
		{"country", "tenant", "main", "content", true, "US", "", "", []string{"country-item", "second-country-item"}, []string{}},
		{"subdivision", "tenant", "main", "entities", true, "", "US-TX", "", []string{}, []string{"tag"}},
		{"address", "tenant", "main", "content", true, "", "", "203.0.113.45/32", []string{"address-item"}, []string{}},
		{"network", "tenant", "main", "content", true, "", "", "203.0.113.0/24", []string{"network-item"}, []string{}},
		{"disabled", "tenant", "main", "site", false, "US", "", "", []string{}, []string{}},
		{"domain", "tenant", "other", "site", true, "US", "", "", []string{}, []string{}},
		{"foreign", "other", "main", "site", true, "US", "", "", []string{}, []string{}},
	} {
		_, err := pool.Exec(t.Context(), `INSERT INTO `+rules+` (tenant_id,id,target,enabled,created_by,updated_by) VALUES ($1,$2,$3,$4,'staff','staff')`, r.tenant, r.id, r.target, r.enabled)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(t.Context(), `INSERT INTO `+quoted+`.content_location_rule_domains (tenant_id,rule_id,domain) VALUES ($1,$2,$3)`, r.tenant, r.id, r.domain)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []struct {
			table, column, parent string
			ids                   []string
		}{
			{"content_location_rule_contents", "content_id", "content_items", r.contents},
			{"content_location_rule_entities", "entity_id", "content_entities", r.tags},
		} {
			if _, err := pool.Exec(t.Context(), `INSERT INTO `+quoted+`.`+target.parent+` SELECT $1,unnest($2::text[]) ON CONFLICT DO NOTHING`, r.tenant, target.ids); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO `+quoted+`.`+target.table+` (tenant_id,rule_id,`+target.column+`) SELECT $1,$2,unnest($3::text[])`, r.tenant, r.id, target.ids); err != nil {
				t.Fatal(err)
			}
		}
		_, err = pool.Exec(t.Context(), `INSERT INTO `+entries+` (tenant_id,rule_id,position,country,subdivision,network) VALUES ($1,$2,0,nullif($3,''),nullif($4,''),nullif($5,'')::cidr)`, r.tenant, r.id, r.country, r.subdivision, r.network)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+quoted+`.content_location_rule_domains VALUES ('tenant','country','another'); INSERT INTO `+entries+` (tenant_id,rule_id,position,country) VALUES ('tenant','country',1,'US')`); err != nil {
		t.Fatal(err)
	}
	// Each request starts its own location state; within one, rules are read once.
	request := func() context.Context {
		return WithLocation(t.Context(), content.VisitorLocation{Country: "US", Subdivision: "US-TX"})
	}
	ctx := request()
	b, err := Resolve(ctx, pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if b.Site || len(b.Matches) != 4 || b.Allows("country-item", nil, "") || b.Allows("second-country-item", nil, "") || b.Allows("address-item", nil, "") || b.Allows("network-item", nil, "") || b.Allows("uncovered", []string{"tag"}, "") || b.Allows("uncovered", nil, "tag") || !b.Allows("uncovered", nil, "") {
		t.Fatalf("incorrect enabled/domain/tenant matching: %+v", b)
	}
	if !b.Ruled || b.SiteRuled || !Varies(ctx) {
		t.Fatalf("rule presence: %+v", b)
	}
	pred, args := b.SQL("f", "location")
	rows, err := pool.Query(ctx, `SELECT f.content_id FROM (VALUES ('country-item','{}'::text[],NULL),('tag-item',ARRAY['tag'],NULL),('owned-item','{}'::text[],'tag'),('uncovered','{}'::text[],NULL)) f(content_id,entity_ids,owner_entity_id) WHERE `+pred, args)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "uncovered" {
		t.Fatalf("SQL boundary leaked rows: %v", ids)
	}
	unknown, err := Resolve(t.Context(), pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Matches) != 2 || !unknown.Allows("country-item", []string{"tag"}, "") {
		t.Fatalf("unknown location matched geography: %+v", unknown)
	}
	reset := WithLocation(ctx, content.VisitorLocation{})
	refreshed, err := Resolve(reset, pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed.Matches) != 2 {
		t.Fatal("explicit new lookup inherited previous rule selection")
	}
	other, err := Resolve(ctx, pool, schema, "tenant", "other", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if !other.Site || len(other.Matches) != 1 {
		t.Fatalf("domain scope lost: %+v", other)
	}
	_, err = pool.Exec(ctx, `UPDATE `+rules+` SET enabled=true WHERE tenant_id='tenant' AND id='disabled'`)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Resolve(request(), pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if !next.Site {
		t.Fatal("next request did not see enabled rule")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM `+quoted+`.content_items WHERE tenant_id='tenant'; DELETE FROM `+quoted+`.content_entities WHERE tenant_id='tenant'; UPDATE `+rules+` SET enabled=false WHERE target='site'`); err != nil {
		t.Fatal(err)
	}
	next, err = Resolve(request(), pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if !next.Empty() || len(next.Matches) != 0 {
		t.Fatalf("rules without targets still matched: %+v", next)
	}
	_, err = pool.Exec(ctx, `DELETE FROM `+rules+` WHERE tenant_id='tenant'`)
	if err != nil {
		t.Fatal(err)
	}
	next, err = Resolve(request(), pool, schema, "tenant", "main", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	if !next.Empty() || next.Ruled {
		t.Fatal("next request retained deleted rules")
	}
}
