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
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/locationpolicy"
	"github.com/open-rails/contentkit/internal/migrations"
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
	setup, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	defer func() {
		setup.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+qs+" CASCADE")
		setup.Exec(context.Background(), "DELETE FROM public.migrations WHERE schema = $1", schema)
	}()
	db := stdlib.OpenDBFromPool(setup)
	defer db.Close()
	if err := migrations.ApplyPostgres(t.Context(), db, schema); err != nil {
		t.Fatal(err)
	}
	tx, err := setup.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SET LOCAL search_path TO "+qs); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO content_items (tenant_id,id,kind,state) VALUES ('tenant','one','article','draft'),('tenant','two','gallery','draft'),('tenant','deleted','article','deleted'),('other','foreign','article','draft'),('other','one','article','draft');
INSERT INTO content_versions (tenant_id,id,content_id,state,position,languages,access) VALUES ('tenant','v-de','one','draft',0,'{de}','{}'),('tenant','v-en','one','draft',1,'{en}','{}'),('tenant','fallback','two','draft',0,'{de}','{}'),('tenant','removed','two','deleted',0,'{en}','{}'),('other','v-foreign','one','draft',0,'{en}','{}');
INSERT INTO content_version_text (tenant_id,version_id,language,title) VALUES ('tenant','v-de','de','German'),('tenant','v-en','en','English'),('tenant','fallback','de','Fallback'),('tenant','removed','en','Deleted version'),('other','v-foreign','en','Foreign title');
INSERT INTO content_entities (tenant_id,id,kind,slug,state) VALUES ('tenant','tag','tag','tag','active'),('tenant','deleted-tag','tag','deleted-tag','deleted'),('other','foreign-tag','tag','foreign-tag','active'),('other','tag','tag','tag','active');
INSERT INTO content_entity_names (tenant_id,entity_id,language,kind,name) VALUES ('tenant','tag','de','name','German tag'),('tenant','tag','en','name','English tag'),('tenant','tag','en','alias','Alias'),('other','tag','en','name','Foreign tag');
INSERT INTO content_location_rules (tenant_id,id,target,enabled,created_by,updated_by) VALUES
('tenant','first','content',true,'staff','staff'),
('tenant','second','content',true,'staff','staff'),
('tenant','third','entities',true,'staff','staff');
INSERT INTO content_location_rule_entries (tenant_id,rule_id,position,country) VALUES ('tenant','first',0,'GB'),('tenant','second',0,'GB'),('tenant','third',0,'GB');
INSERT INTO content_location_rule_domains VALUES ('tenant','first','z'),('tenant','second','z'),('tenant','third','z');
INSERT INTO content_location_rule_contents VALUES ('tenant','first','two'),('tenant','first','one'),('tenant','first','deleted'),('tenant','second','one');
INSERT INTO content_location_rule_entities VALUES ('tenant','third','tag'),('tenant','third','deleted-tag');
DELETE FROM content_items WHERE tenant_id='tenant' AND id='deleted';
DELETE FROM content_entities WHERE tenant_id='tenant' AND id='deleted-tag';`); err != nil {
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
		"first":  {{ID: "one", Kind: "article", Title: "English"}, {ID: "two", Kind: "gallery", Title: "Fallback"}},
		"second": {{ID: "one", Kind: "article", Title: "English"}},
		"third":  {{ID: "tag", Kind: "tag", Name: "English tag"}},
	}
	for _, r := range page.Items {
		if r.UpdatedByName != "" {
			t.Fatalf("name without directory: %+v", r)
		}
		if !slices.Equal(r.Domains, []string{"z"}) {
			t.Fatalf("rule domains: %+v", r)
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
	for _, tc := range []struct {
		name, target, message string
		contents, entities    []string
	}{
		{"site-content", "site", "site target has no content or entity ids", []string{"one"}, nil},
		{"site-entity", "site", "site target has no content or entity ids", nil, []string{"tag"}},
		{"empty-content", "content", "content target needs content ids only", nil, nil},
		{"mixed-content", "content", "content target needs content ids only", []string{"one"}, []string{"tag"}},
		{"empty-entities", "entities", "entities target needs entity ids only", nil, nil},
		{"mixed-entities", "entities", "entities target needs entity ids only", []string{"one"}, []string{"tag"}},
		{"unknown-target", "unknown", "target must be site, content or entities", nil, nil},
		{"foreign-content", "content", "target ids must exist in this tenant", []string{"foreign"}, nil},
		{"foreign-entity", "entities", "target ids must exist in this tenant", nil, []string{"foreign-tag"}},
		{"missing-content", "content", "target ids must exist in this tenant", []string{"missing"}, nil},
		{"missing-entity", "entities", "target ids must exist in this tenant", nil, []string{"missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.saveLocationRule(ctx, "staff", "", &content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: tc.target, ContentIDs: tc.contents, EntityIDs: tc.entities, Domains: []string{"z"}}, nil, false)
			if err == nil || err.Error() != tc.message {
				t.Fatalf("save error: %v, want %q", err, tc.message)
			}
		})
	}
	siteTarget := "site"
	if _, err := rt.saveLocationRule(ctx, "staff", "first", nil, &content.LocationRuleUpdate{Target: &siteTarget}, false); err == nil || err.Error() != "site target has no content or entity ids" {
		t.Fatalf("invalid patch: %v", err)
	}
	unchanged, err := rt.locationRules(ctx, 100, 0)
	if err != nil || unchanged.Total != 3 {
		t.Fatalf("invalid saves changed rules: %+v, err=%v", unchanged, err)
	}
	for _, r := range unchanged.Items {
		if r.ID == "first" && (r.Target != "content" || !slices.Equal(r.ContentIDs, []string{"one", "two"})) {
			t.Fatalf("invalid patch changed rule: %+v", r)
		}
	}
	failedHistory, err := rt.locationHistory(ctx, "", 100, 0)
	if err != nil || failedHistory.Total != 0 {
		t.Fatalf("invalid saves wrote history: %+v, err=%v", failedHistory, err)
	}
	// Mutation responses use the same labels; snapshots retain the old shape.
	lookupCalls = 0
	created, err := rt.saveLocationRule(ctx, "staff", "", &content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "GB"}}, Target: "content", ContentIDs: []string{"two", "one", "one"}, Domains: []string{"z", "a", "z"}, Enabled: true}, nil, false)
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
	if !slices.Equal(created.ContentIDs, []string{"one", "two"}) || !slices.Equal(created.Domains, []string{"a", "z"}) {
		t.Fatalf("create sets: %+v", created)
	}
	var domainRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.content_location_rule_domains WHERE tenant_id='tenant' AND rule_id=$1`, created.ID).Scan(&domainRows); err != nil || domainRows != 2 {
		t.Fatalf("domain rows: %d, err=%v", domainRows, err)
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
	// A patch that waits for a prior writer must read its committed child rows.
	writer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	prior, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer prior.Rollback(context.Background())
	if _, err := prior.Exec(ctx, `SELECT id FROM `+qs+`.content_location_rules WHERE tenant_id='tenant' AND id='first' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := prior.Exec(ctx, `DELETE FROM `+qs+`.content_location_rule_contents WHERE tenant_id='tenant' AND rule_id='first' AND content_id='one'`); err != nil {
		t.Fatal(err)
	}
	var patchPID int
	if err := pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&patchPID); err != nil {
		t.Fatal(err)
	}
	patchCtx, cancelPatch := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPatch()
	patchDone := make(chan error, 1)
	go func() {
		note := "after concurrent target update"
		r, err := rt.saveLocationRule(patchCtx, "staff", "first", nil, &content.LocationRuleUpdate{Note: &note}, false)
		if err == nil && !slices.Equal(r.ContentIDs, []string{"two"}) {
			err = fmt.Errorf("waiting patch restored stale targets: %v", r.ContentIDs)
		}
		patchDone <- err
	}()
	for {
		var waiting bool
		if err := prior.QueryRow(patchCtx, `SELECT cardinality(pg_blocking_pids($1))>0`, patchPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := prior.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-patchDone; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM `+qs+`.content_items WHERE tenant_id='tenant'; DELETE FROM `+qs+`.content_entities WHERE tenant_id='tenant'`); err != nil {
		t.Fatal(err)
	}
	page, err = rt.locationRules(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 {
		t.Fatalf("target deletion removed rules: %+v", page)
	}
	for _, r := range page.Items {
		if !r.Enabled || r.ContentIDs == nil || r.EntityIDs == nil || r.Targets == nil || len(r.ContentIDs)+len(r.EntityIDs)+len(r.Targets) != 0 {
			t.Fatalf("target deletion left ids or labels: %+v", r)
		}
	}
	afterCascade, err := rt.locationHistory(ctx, created.ID, 100, 0)
	if err != nil || afterCascade.Total != history.Total || !reflect.DeepEqual(afterCascade.Items[0].Snapshot, history.Items[0].Snapshot) || !reflect.DeepEqual(afterCascade.Items[1].Snapshot, history.Items[1].Snapshot) {
		t.Fatalf("cascade changed history: %+v, err=%v", afterCascade, err)
	}
	if _, err := rt.saveLocationRule(ctx, "staff", created.ID, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rule_contents", "rule_entities", "rule_domains", "rule_entries"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.locationTable(table)+` WHERE tenant_id='tenant' AND rule_id=$1`, created.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rule deletion left %s rows: %d, err=%v", table, n, err)
		}
	}
	afterDelete, err := rt.locationHistory(ctx, created.ID, 100, 0)
	if err != nil || afterDelete.Total != 3 || afterDelete.Items[0].Action != "delete" {
		t.Fatalf("rule deletion lost history: %+v, err=%v", afterDelete, err)
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
