package taxonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every admin route is driven live with a positive and a negative case.
func TestHandlerIntegration(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	srv := httptest.NewServer(Handler(s))
	t.Cleanup(srv.Close)
	indexVersions(t, ctx, pool, schema, tenant, version{"gallery", "g1", "v1", "en", "Blue Ocean", true, true})

	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}
	expect := func(method, path, body string, status int, contains string) string {
		t.Helper()
		code, out := call(method, path, body)
		if code != status || !strings.Contains(out, contains) {
			t.Fatalf("%s %s -> %d %s (want %d containing %q)", method, path, code, out, status, contains)
		}
		return out
	}

	expect("POST", "/nodes", `[{"taxonomy_id":"colored","kind":"tag","slug":"colored","names":[{"language":"en","kind":"name","name":"Colored"}]},{"taxonomy_id":"colour","kind":"tag","slug":"colour"},{"taxonomy_id":"fate","kind":"series","slug":"fate"}]`, http.StatusCreated, `"taxonomy_id":"colored"`)
	expect("POST", "/nodes", `[{"kind":"tag","slug":"colored"}]`, http.StatusConflict, "conflict")
	expect("POST", "/nodes", `[{"kind":"studio","slug":"x"}]`, http.StatusBadRequest, "not registered")
	expect("POST", "/nodes", `[{"kind":"tag","slug":"x","unknown":1}]`, http.StatusBadRequest, "unknown field")
	expect("GET", "/nodes?kind=tag&limit=1", "", http.StatusOK, `"next_cursor"`)
	expect("GET", "/nodes?kind=studio", "", http.StatusBadRequest, "not registered")
	expect("GET", "/nodes/colored", "", http.StatusOK, `"name":"Colored"`)
	expect("GET", "/nodes/missing", "", http.StatusNotFound, "not found")
	expect("PATCH", "/nodes/colored", `{"slug":"full-color"}`, http.StatusOK, `"slug":"full-color"`)
	expect("PATCH", "/nodes/colored", `{"state":"merged"}`, http.StatusBadRequest, "not assignable")
	expect("PUT", "/nodes/colored/names", `[{"language":"en","kind":"name","name":"Full Color"},{"language":"es","kind":"alias","name":"Coloreado"}]`, http.StatusOK, `"name":"Coloreado"`)
	expect("POST", "/nodes/colored/names", `[{"language":"en","kind":"alias","name":"Colored"}]`, http.StatusOK, `"name":"Colored"`)
	expect("DELETE", "/nodes/colored/names", `[{"language":"es","name":"Coloreado"}]`, http.StatusOK, `"names":[{"language":"en","kind":"name","name":"Full Color"`)
	expect("POST", "/nodes/missing/names", `[{"language":"en","name":"x"}]`, http.StatusNotFound, "not found")
	expect("POST", "/edges", `[{"from_taxonomy_id":"colored","relation":"synonym","to_taxonomy_id":"colour"}]`, http.StatusOK, `"edges":1`)
	expect("POST", "/edges", `[{"from_taxonomy_id":"colored","relation":"synonym","to_taxonomy_id":"colored"}]`, http.StatusBadRequest, "self loop")
	expect("POST", "/edges", `[{"from_taxonomy_id":"colored","relation":"synonym","to_taxonomy_id":"missing"}]`, http.StatusNotFound, "not found")
	expect("POST", "/assignments", `[{"content_kind":"gallery","content_id":"g1","taxonomy_id":"colored"},{"content_kind":"gallery","content_id":"g1","content_version_id":"v1","taxonomy_id":"colour","relation":"tag"}]`, http.StatusOK, `"assignments":2`)
	expect("POST", "/assignments", `[{"content_kind":"gallery","content_id":"g1","taxonomy_id":"missing"}]`, http.StatusNotFound, "not active")
	expect("POST", "/assignments", `[{"tenant_id":"hentai0","content_kind":"video","content_id":"m1","taxonomy_id":"colored"}]`, http.StatusBadRequest, "outside tenant")
	out := expect("POST", "/effective", `[{"content_kind":"gallery","content_id":"g1","content_version_id":"v1"},{"content_kind":"gallery","content_id":"g1"}]`, http.StatusOK, `"scope":"version"`)
	var effective []EffectiveTagsOf
	if err := json.Unmarshal([]byte(out), &effective); err != nil || len(effective) != 2 || len(effective[0].Tags) != 2 || len(effective[1].Tags) != 1 || effective[1].Content.TenantID != tenant {
		t.Fatalf("effective: %s %v", out, err)
	}
	expect("GET", "/counts?taxonomy_id=colored&taxonomy_id=colour", "", http.StatusOK, `"colour":[{"content_kind":"gallery","language":"en","count":1}]`)
	expect("DELETE", "/assignments", `[{"content_kind":"gallery","content_id":"g1","content_version_id":"v1","taxonomy_id":"colour"}]`, http.StatusOK, `"assignments":1`)
	expect("GET", "/counts?taxonomy_id=colour", "", http.StatusOK, `{}`)
	expect("POST", "/assignments?suppress_counts=true", `[{"content_kind":"gallery","content_id":"g1","taxonomy_id":"colour"}]`, http.StatusOK, `"assignments":1`)
	expect("GET", "/counts?taxonomy_id=colour", "", http.StatusOK, `{}`)
	expect("POST", "/counts/rebuild", "", http.StatusOK, `"nodes":3`)
	expect("GET", "/counts?taxonomy_id=colour", "", http.StatusOK, `"count":1`)
	expect("POST", "/nodes/colour/merge", `{"into_taxonomy_id":"colour"}`, http.StatusBadRequest, "itself")
	expect("POST", "/nodes/colour/merge", `{"into_taxonomy_id":"fate"}`, http.StatusConflict, "kind")
	expect("POST", "/nodes/colour/merge", `{"into_taxonomy_id":"colored"}`, http.StatusOK, `"assignments_merged":1`)
	expect("GET", "/nodes/colour", "", http.StatusOK, `"state":"merged"`)
	expect("PATCH", "/nodes/colour", `{"slug":"x"}`, http.StatusConflict, "merged")
	expect("DELETE", "/nodes/colored", "", http.StatusOK, `"state":"deleted"`)
	expect("DELETE", "/nodes/missing", "", http.StatusNotFound, "not found")
	expect("GET", "/counts?taxonomy_id=colored", "", http.StatusOK, `{}`)
	expect("DELETE", "/edges", `[{"from_taxonomy_id":"colour","relation":"alias_of","to_taxonomy_id":"colored"}]`, http.StatusOK, `"edges":1`)
	expect("GET", "/nodes/colour", "", http.StatusOK, `"edges":[]`)
}
