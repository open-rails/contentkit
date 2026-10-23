package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/testauth"
)

type managementRoles struct{}

func (managementRoles) Can(_ context.Context, a access.Actor, permission string) (bool, error) {
	switch a.ID {
	case "staff":
		return true, nil
	case "content-staff":
		return permission == string(access.ContentWrite), nil
	case "entity-staff":
		return permission == string(access.EntitiesWrite), nil
	case "auditor":
		return permission == string(access.ContentViewRestricted), nil
	}
	return false, nil
}

func managementFixture(t *testing.T) (*fixture, string) {
	f := seeded(t)
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	creators := []catalog.Creator{{UserID: "alice"}}
	ids, err := f.cat.Create(f.ctx, staff, []catalog.ContentInput{{Kind: "gallery", Creators: &creators,
		Text: map[string]catalog.Text{"en": {Title: "Private Azure Draft", Aliases: []string{"The Azure Alias"}, Body: "Bleach"}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Public documents keep approved text; operator names reflect the current edit.
	f.exec(`UPDATE {s}.content_text SET title='Azure Revision' WHERE tenant_id=$1 AND content_id=$2 AND language='en'`, tenant, f.cid(1))
	f.exec(`UPDATE {s}.content_items SET moderation='held',published_snapshot='{"en":{"title":"Alpha"}}'::jsonb WHERE tenant_id=$1 AND id=$2`, tenant, f.cid(1))
	f.entity("series", "bleach")
	f.entity("tag", "azure-marker")
	internal := true
	if _, err = f.entities.UpdateEntities(f.ctx, []entities.EntityUpdate{{ID: entities.EntityID(f.ids["azure-marker"]), Internal: &internal}}); err != nil {
		t.Fatal(err)
	}
	f.seed(item{n: 20, titles: map[string]string{"en": "Unrelated Gallery"}, entities: []string{"bleach"}, versions: []ver{{langs: []string{"en"}}}})
	provider := testauth.Auth{Authz: managementRoles{}}
	cat, err := catalog.New(f.ctx, catalog.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: f.svc.o.Kinds, Auth: provider, Entities: f.entities})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	f.svc.o.Visibility = cat
	f.h, err = f.svc.Handler(provider)
	if err != nil {
		t.Fatal(err)
	}
	return f, ids[0]
}

func managementPath(query string) string {
	return q("/manage/search-suggestions", "q", query, "include_contents", "true", "include_entities", "true")
}
func suggestionIDs(out Typeahead) []string {
	ids := []string{}
	for _, card := range out.Contents {
		ids = append(ids, card.ID)
	}
	return ids
}

func TestManagementSuggestionsUseCurrentPrivateNames(t *testing.T) {
	f, draft := managementFixture(t)
	var out Typeahead
	if status, code := f.call(http.MethodGet, "gay.test", "staff", managementPath("azure"), nil, &out); status != http.StatusOK {
		t.Fatalf("%d %s", status, code)
	}
	if !reflect.DeepEqual(suggestionIDs(out), []string{f.cid(1), draft}) || len(out.Entities) != 1 || string(out.Entities[0].ID) != f.ids["azure-marker"] || !out.Entities[0].Internal {
		t.Fatalf("current draft/held/internal names missing: %+v", out)
	}
	if out.Contents[0].Title != "Azure Revision" || out.Contents[1].Title != "Private Azure Draft" || len(out.Contents[0].Cover) != 0 {
		t.Fatalf("operator targets are lean current names: %+v", out)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "staff", managementPath("bleach"), nil, &out); status != http.StatusOK || len(out.Contents) != 0 || len(out.Entities) != 1 || string(out.Entities[0].ID) != f.ids["bleach"] {
		t.Fatalf("associated entity/body leaked into name suggestions: %+v", out)
	}
	if status, _ := f.call(http.MethodGet, "gay.test", "staff", managementPath("bravo"), nil, &out); status != http.StatusOK || !reflect.DeepEqual(suggestionIDs(out), []string{f.cid(2)}) {
		t.Fatalf("reader/domain filters were applied to management: %+v", out)
	}
	staff := testauth.Actor(access.Actor{ID: "staff"}, staffOnly{})
	jp, err := f.cat.Create(f.ctx, staff, []catalog.ContentInput{{Kind: "gallery", Text: map[string]catalog.Text{"ja": {Title: "稲妻の記録"}}}})
	if err != nil {
		t.Fatal(err)
	}
	jpEntities, err := f.entities.CreateEntities(f.ctx, []entities.EntityInput{{Kind: "tag", Slug: "jp-managed", Internal: true, Names: []entities.Name{{Language: "ja", Kind: entities.NameCanonical, Name: "稲妻"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "staff", managementPath("稲妻"), nil, &out); status != http.StatusOK || !reflect.DeepEqual(suggestionIDs(out), []string{jp[0]}) || out.Contents[0].Language != "ja" || len(out.Entities) != 1 || out.Entities[0].ID != jpEntities[0].ID {
		t.Fatalf("display language incorrectly hid editable Japanese-only names: %+v", out)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "staff", managementPath("the a an"), nil, &out); status != http.StatusOK || len(out.Contents)+len(out.Entities) != 0 {
		t.Fatalf("filler-only management became a wildcard: %+v", out)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "", q("/typeahead", "q", "azure"), nil, &out); status != http.StatusOK || len(out.Contents)+len(out.Entities) != 0 {
		t.Fatalf("private current names leaked into public search: %+v", out)
	}
	for _, path := range []string{
		q("/manage/search-suggestions", "q", "azure"), q("/manage/search-suggestions", "q", "azure", "include_contents", "true", "limit", "21"),
		q("/manage/search-suggestions", "q", "azure", "include_entities", "true", "entity_kinds", "missing"),
		q("/manage/search-suggestions", "q", "azure", "include_contents", "invalid"),
		q("/manage/search-suggestions", "q", strings.Repeat("x", 257), "include_contents", "true"),
	} {
		if status, code := f.call(http.MethodGet, "main.test", "staff", path, nil, nil); status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%s: %d %s", path, status, code)
		}
	}
}

func TestManagementSuggestionsCheckSectionsAndTenantIdentity(t *testing.T) {
	f, draft := managementFixture(t)
	for _, tc := range []struct {
		who      string
		contents []string
		entities int
	}{
		{"staff", []string{f.cid(1), draft}, 1}, {"content-staff", []string{f.cid(1), draft}, 0}, {"entity-staff", []string{}, 1}, {"alice", []string{draft}, 0}, {"auditor", []string{}, 0},
	} {
		var out Typeahead
		if status, code := f.call(http.MethodGet, "main.test", tc.who, managementPath("azure"), nil, &out); status != http.StatusOK || !reflect.DeepEqual(suggestionIDs(out), tc.contents) || len(out.Entities) != tc.entities {
			t.Errorf("%s: status=%d code=%s out=%+v", tc.who, status, code, out)
		}
	}
	var out Typeahead
	if status, code := f.call(http.MethodGet, "main.test", "", managementPath("azure"), nil, nil); status != http.StatusUnauthorized || code != "unauthorized" {
		t.Fatalf("anonymous management: %d %s", status, code)
	}
	if status, code := f.call(http.MethodGet, "main.test", "alice", q("/manage/search-suggestions", "q", "azure", "include_entities", "true"), nil, nil); status != http.StatusForbidden || code != "forbidden" {
		t.Fatalf("entity-only unauthorized: %d %s", status, code)
	}
	if status, _ := f.call(http.MethodGet, "main.test", "alice", q("/manage/search-suggestions", "q", "azure", "include_contents", "true", "limit", "1"), nil, &out); status != http.StatusOK || !reflect.DeepEqual(suggestionIDs(out), []string{draft}) {
		t.Fatalf("creator policy ran after result limit: %+v", out)
	}
	foreign := testauth.Actor(access.Actor{ID: "alice", Account: &access.AccountMembership{}}, managementRoles{})
	got, err := f.svc.ManagementSuggestions(f.ctx, ManagementSuggestionsOptions{Actor: foreign, Query: "azure", IncludeContents: true})
	if !errors.Is(err, auth.ErrForbidden) || len(got.Contents) != 0 {
		t.Fatalf("foreign source identity reused local creator: %+v %v", got, err)
	}
	// Application credentials are admitted by permission, not confused with audience accounts.
	request := httptest.NewRequest(http.MethodGet, "http://main.test"+managementPath("azure"), nil)
	request.Header.Set("Authorization", "Bearer app:content-staff")
	recorder := httptest.NewRecorder()
	f.h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized application: %d %s", recorder.Code, recorder.Body.String())
	}
	if err = json.Unmarshal(recorder.Body.Bytes(), &out); err != nil || len(out.Contents) != 2 || len(out.Entities) != 0 {
		t.Fatalf("application scope: %+v %v", out, err)
	}
}

type managementAppAuth struct {
	testauth.Auth
	app string
}

func (a managementAppAuth) ResolveActor(_ context.Context, v auth.Verified) (string, error) {
	return v.Identity().Subject, nil
}
func (a managementAppAuth) Authorize(_ context.Context, v auth.Verified, _ string, permission string) (bool, error) {
	return v.Identity().Subject == a.app+"-staff" && (permission == string(access.ContentWrite) || permission == string(access.EntitiesWrite)), nil
}

func TestManagementSuggestionsRejectOtherAppGrants(t *testing.T) {
	f, _ := managementFixture(t)
	provider := managementAppAuth{app: "doujins"}
	cat, err := catalog.New(f.ctx, catalog.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: f.svc.o.Kinds, Auth: provider, Entities: f.entities})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	f.svc.o.Visibility = cat
	f.h, err = f.svc.Handler(provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		who  string
		want int
	}{{"doujins-staff", 2}, {"hentai0-staff", 0}} {
		var out Typeahead
		if status, _ := f.call(http.MethodGet, "main.test", tc.who, managementPath("azure"), nil, &out); status != http.StatusOK || len(out.Contents) != tc.want {
			t.Fatalf("%s app scope: %d %+v", tc.who, status, out)
		}
	}
}

func TestManagementAliasRowsEntityBeforeSuggestionLimit(t *testing.T) {
	f, _ := managementFixture(t)
	f.entity("tag", "azure-second")
	f.exec(`INSERT INTO {s}.content_entity_names(tenant_id,entity_id,language,kind,name)
 SELECT $1,$2,'en','alias','Azure variant '||i::text FROM generate_series(1,200) i`, tenant, f.ids["azure-marker"])
	// All current-name locales are searched even when the display language is
	// English. Translated aliases must share the same target budget too.
	f.exec(`INSERT INTO {s}.content_entity_names(tenant_id,entity_id,language,kind,name)
 SELECT $1,$2,'x-'||lpad(i::text,3,'0'),'alias','Azure localized variant' FROM generate_series(1,20) i`, tenant, f.ids["azure-marker"])
	var out Typeahead
	path := q("/manage/search-suggestions", "q", "azure", "include_entities", "true", "entity_kinds", "tag", "limit", "2")
	if status, _ := f.call(http.MethodGet, "main.test", "staff", path, nil, &out); status != http.StatusOK || len(out.Entities) != 2 || out.Entities[0].ID == out.Entities[1].ID {
		t.Fatalf("one entity's aliases consumed the result limit: %+v", out)
	}
}

func TestManagementNameViewsUseNativeIndexes(t *testing.T) {
	f, _ := managementFixture(t)
	// Real current-text/name tables, without search-worker backfill. The planner
	// must select the new native expressions without disabling sequential scans.
	f.exec(`INSERT INTO {s}.content_items(tenant_id,id,kind)
 SELECT $1,'01940000-0000-7000-8000-'||lpad(i::text,12,'0'),'gallery' FROM generate_series(1,20000) i`, tenant)
	f.exec(`INSERT INTO {s}.content_text(tenant_id,content_id,language,title)
 SELECT tenant_id,id,'en','Filler '||md5(id) FROM {s}.content_items WHERE tenant_id=$1 AND id LIKE '01940000-%'`, tenant)
	f.exec(`INSERT INTO {s}.content_entity_names(tenant_id,entity_id,language,kind,name)
 SELECT $1,$2,'en','alias','Filler '||md5(i::text) FROM generate_series(1,20000) i`, tenant, f.ids["azure-marker"])
	f.exec(`ANALYZE {s}.content_text`)
	f.exec(`ANALYZE {s}.content_items`)
	f.exec(`ANALYZE {s}.content_entity_names`)
	f.exec(`ANALYZE {s}.content_entities`)
	for _, tc := range []struct{ view, predicate, index string }{
		{"content_management_content_names", fmt.Sprintf(`%s(sd.title,sd.aliases) LIKE '%%zure%%'`, f.table("contentkit_search_names")), "content_text_management_substring"},
		{"content_management_content_names", fmt.Sprintf(`%s(sd.title,sd.aliases,'{}'::text[],'') OPERATOR(public.&^) 'private'`, f.table("contentkit_search_terms")), "content_text_management_prefix"},
		{"content_management_entity_names", fmt.Sprintf(`%s(sd.title,sd.aliases) LIKE '%%zure%%'`, f.table("contentkit_search_names")), "content_entity_names_management_substring"},
		{"content_management_entity_names", fmt.Sprintf(`%s(sd.title,sd.aliases,'{}'::text[],'') OPERATOR(public.&^) 'azure'`, f.table("contentkit_search_terms")), "content_entity_names_management_prefix"},
	} {
		rows, err := f.pool.Query(f.ctx, `EXPLAIN (ANALYZE,BUFFERS,COSTS OFF) SELECT sd.content_id FROM `+f.table(tc.view)+` sd WHERE sd.tenant_id=$1 AND sd.language='en' AND `+tc.predicate+` LIMIT 20`, tenant)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", tc.index, plan.String())
		sourceTable := "content_text"
		if tc.view == "content_management_entity_names" {
			sourceTable = "content_entity_names"
		}
		if !strings.Contains(plan.String(), tc.index) || strings.Contains(plan.String(), "Seq Scan on "+sourceTable) {
			t.Fatalf("management lookup must use %s:\n%s", tc.index, plan.String())
		}
	}
}
