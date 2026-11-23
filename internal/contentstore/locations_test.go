package contentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/locationpolicy"
	"github.com/open-rails/contentkit/kinds"
)

type locationQueryTrace struct{ queries []string }

func (tr *locationQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.queries = append(tr.queries, data.SQL)
	return ctx
}
func (*locationQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type locationDirectory struct {
	userinfo.Lookup
	get func(context.Context, []string) (map[string]userinfo.User, error)
}

func (d locationDirectory) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	return d.get(ctx, ids)
}

func TestLocationRulesFromPostgres(t *testing.T) {
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL is required for the database location-rule test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	trace := &locationQueryTrace{}
	cfg.ConnConfig.Tracer = trace
	// Also proves the page's rows release their connection before label queries.
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("ck_location_labels_%d", time.Now().UnixNano())
	qs := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE SCHEMA "+qs); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DROP SCHEMA "+qs+" CASCADE")
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SET LOCAL search_path TO "+qs); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../migrations/postgres/0070_location_rules.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
CREATE TABLE content_items (tenant_id text,id text,kind text,state text);
CREATE TABLE content_versions (tenant_id text,id text,content_id text,state text,position int);
CREATE TABLE content_version_text (tenant_id text,version_id text,language text,title text);
CREATE TABLE content_entities (tenant_id text,id text,kind text,state text);
CREATE TABLE content_entity_names (tenant_id text,entity_id text,language text,kind text,name text);
CREATE TABLE content_erased_subjects (tenant_id text,actor_id text);
INSERT INTO content_items VALUES ('tenant','one','article','draft'),('tenant','two','gallery','draft'),('tenant','deleted','article','deleted'),('other','foreign','article','draft');
INSERT INTO content_versions VALUES ('tenant','v-de','one','draft',0),('tenant','v-en','one','draft',1),('tenant','fallback','two','draft',0),('tenant','removed','two','deleted',0),('other','v-foreign','one','draft',0);
INSERT INTO content_version_text VALUES ('tenant','v-de','de','German'),('tenant','v-en','en','English'),('tenant','fallback','de','Fallback'),('tenant','removed','en','Deleted version'),('other','v-foreign','en','Foreign title');
INSERT INTO content_entities VALUES ('tenant','tag','tag','active'),('tenant','deleted-tag','tag','deleted'),('other','foreign-tag','tag','active');
INSERT INTO content_entity_names VALUES ('tenant','tag','de','name','German tag'),('tenant','tag','en','name','English tag'),('tenant','tag','en','alias','Alias'),('other','tag','en','name','Foreign tag');
INSERT INTO content_location_rules (tenant_id,id,target,content_ids,entity_ids,domains,enabled,created_by,updated_by) VALUES
('tenant','first','content',ARRAY['two','one','deleted','missing','foreign'],'{}',ARRAY['z'],true,'staff','staff'),
('tenant','second','content',ARRAY['one'],'{}',ARRAY['z'],true,'staff','staff'),
('tenant','third','entities','{}',ARRAY['tag','deleted-tag','missing-tag','foreign-tag'],ARRAY['z'],true,'staff','staff');`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{store: newStore(pool, schema, "tenant"), schema: schema, tenant: "tenant", cfg: kinds.Config{Languages: []string{"en", "de"}, ContentFilters: kinds.ContentFilters{Domains: []kinds.Domain{{Name: "z"}, {Name: "a"}}}}}
	ctx := locationpolicy.WithBlock(t.Context(), "tenant", "z", locationpolicy.Block{Site: true, ContentIDs: []string{"one", "two"}, EntityIDs: []string{"tag"}})
	trace.queries = nil
	page, err := rt.locationRules(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(page.Domains, []string{"z", "a"}) || page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("page: %+v", page)
	}
	want := map[string][]content.LocationRuleTarget{
		"first":  {{ID: "two", Kind: "gallery", Title: "Fallback"}, {ID: "one", Kind: "article", Title: "English"}, {ID: "deleted"}, {ID: "missing"}, {ID: "foreign"}},
		"second": {{ID: "one", Kind: "article", Title: "English"}},
		"third":  {{ID: "tag", Kind: "tag", Name: "English tag"}, {ID: "deleted-tag"}, {ID: "missing-tag"}, {ID: "foreign-tag"}},
	}
	for _, r := range page.Items {
		if r.UpdatedByName != "" {
			t.Fatalf("name without directory: %+v", r)
		}
		if !reflect.DeepEqual(r.Targets, want[r.ID]) {
			t.Fatalf("rule %s targets: %+v, want %+v", r.ID, r.Targets, want[r.ID])
		}
	}
	contentQueries, entityQueries := 0, 0
	for _, sql := range trace.queries {
		if strings.HasPrefix(sql, "SELECT c.id,c.kind") {
			contentQueries++
		}
		if strings.HasPrefix(sql, "SELECT e.id,e.kind") {
			entityQueries++
		}
	}
	if contentQueries != 1 || entityQueries != 1 {
		t.Fatalf("label queries per page: content=%d entities=%d", contentQueries, entityQueries)
	}
	lookupCalls := 0
	rt.userInfo = locationDirectory{get: func(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
		lookupCalls++
		if !slices.Equal(ids, []string{"staff"}) {
			t.Fatalf("directory ids: %v", ids)
		}
		// The directory may itself use the database; no rows or mutation may hold
		// this fixture's only connection during the lookup.
		if err := pool.Ping(ctx); err != nil {
			return nil, err
		}
		return map[string]userinfo.User{"staff": {Name: "Staff Name", Username: "staff-user"}}, nil
	}}
	page, err = rt.locationRules(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lookupCalls != 1 {
		t.Fatalf("list directory calls: %d", lookupCalls)
	}
	for _, r := range page.Items {
		if r.UpdatedByName != "Staff Name" {
			t.Fatalf("rule name: %+v", r)
		}
	}
	// Mutation responses use the same labels; snapshots retain the old shape.
	lookupCalls = 0
	created, err := rt.saveLocationRule(ctx, "staff", "", &content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "content", ContentIDs: []string{"one", "two"}, Domains: []string{"z"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := []content.LocationRuleTarget{{ID: "one", Kind: "article", Title: "English"}, {ID: "two", Kind: "gallery", Title: "Fallback"}}
	if !reflect.DeepEqual(created.Targets, wantTargets) {
		t.Fatalf("create targets: %+v", created)
	}
	if created.UpdatedByName != "Staff Name" || lookupCalls != 1 {
		t.Fatalf("create name: %+v, calls=%d", created, lookupCalls)
	}
	entityIDs, contentIDs, target := []string{"tag"}, []string{}, "entities"
	lookupCalls = 0
	updated, err := rt.saveLocationRule(ctx, "staff", created.ID, nil, &content.LocationRuleUpdate{Target: &target, EntityIDs: &entityIDs, ContentIDs: &contentIDs}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Targets, want["third"][:1]) {
		t.Fatalf("update targets: %+v", updated)
	}
	if updated.UpdatedByName != "Staff Name" || lookupCalls != 1 {
		t.Fatalf("update name: %+v, calls=%d", updated, lookupCalls)
	}
	lookupCalls = 0
	history, err := rt.locationHistory(ctx, created.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 2 || lookupCalls != 1 {
		t.Fatalf("history: %+v", history)
	}
	for _, h := range history.Items {
		if h.ActorName != "Staff Name" {
			t.Fatalf("history actor name: %+v", h)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(h.Snapshot, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["targets"]; exists {
			t.Fatalf("snapshot gained targets: %s", h.Snapshot)
		}
		if _, exists := fields["updated_by_name"]; exists {
			t.Fatalf("snapshot gained name: %s", h.Snapshot)
		}
	}
	for _, tc := range []struct {
		name  string
		users map[string]userinfo.User
		err   error
		want  string
	}{
		{name: "username", users: map[string]userinfo.User{"staff": {Username: "staff-user"}}, want: "staff-user"},
		{name: "unnamed", users: map[string]userinfo.User{"staff": {}}},
		{name: "missing"},
		{name: "failure", err: errors.New("directory unavailable")},
		{name: "unconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookupCalls = 0
			rt.log = slog.New(slog.NewTextHandler(io.Discard, nil))
			rt.userInfo = locationDirectory{get: func(context.Context, []string) (map[string]userinfo.User, error) {
				lookupCalls++
				return tc.users, tc.err
			}}
			wantCalls := 1
			if tc.name == "unconfigured" {
				rt.userInfo = nil
				wantCalls = 0
			}
			page, err := rt.locationRules(ctx, 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Items {
				if r.UpdatedByName != tc.want {
					t.Fatalf("rule name: %+v, want %q", r, tc.want)
				}
			}
			history, err := rt.locationHistory(ctx, "", 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range history.Items {
				if h.ActorName != tc.want {
					t.Fatalf("history name: %+v, want %q", h, tc.want)
				}
			}
			if lookupCalls != 2*wantCalls {
				t.Fatalf("list and history directory calls: %d", lookupCalls)
			}
		})
	}
	rt.cfg.ContentFilters.Domains = nil
	page, err = rt.locationRules(ctx, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(page.Domains, []string{"main"}) || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("empty page default domains: %+v", page)
	}
}
