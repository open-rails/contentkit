package groups

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/internal/testauth"
)

// Every failure answers its stable code and never a SQL identifier; staff
// routes refuse people without the permission.
func TestHandler(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	staff, err := f.s.Handler(testauth.Auth{Authz: testauth.All{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(staff)
	t.Cleanup(srv.Close)
	call := func(bearer, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}
	codes := map[int]string{400: "invalid_request", 401: "unauthorized", 404: "not_found", 409: "conflict"}
	expect := func(bearer, method, path, body string, status int, contains string) string {
		t.Helper()
		code, out := call(bearer, method, path, body)
		if code != status || !strings.Contains(out, contains) {
			t.Fatalf("%s %s -> %d %s (want %d containing %q)", method, path, code, out, status, contains)
		}
		if status >= 400 {
			for _, leak := range []string{"_idx", "_fkey", "_check", "sqlstate", "content_groups", "content_memberships", "pgx"} {
				if strings.Contains(out, leak) {
					t.Fatalf("%s %s leaks %q: %s", method, path, leak, out)
				}
			}
			var e errorBody
			if err := json.Unmarshal([]byte(out), &e); err != nil || e.Code != codes[status] {
				t.Fatalf("%s %s error body %s", method, path, out)
			}
		}
		return out
	}
	const user = "user:u1"
	out := expect(user, "POST", "/groups", `[{"kind":"tag","slug":"color","names":[{"language":"en","kind":"name","name":"Color"}]},{"kind":"series","slug":"saga"}]`, 201, `"slug":"color"`)
	var made []Group
	if err := json.Unmarshal([]byte(out), &made); err != nil || len(made) != 2 {
		t.Fatal(out, err)
	}
	color, saga := string(made[0].ID), string(made[1].ID)
	expect("", "POST", "/groups", `[]`, 401, "")
	expect(user, "POST", "/groups", `[{"kind":"tag","slug":"color"}]`, 409, `"error":"groups: conflict"`)
	expect(user, "POST", "/groups", `[{"kind":"tag","slug":"x","nope":1}]`, 400, "unknown field")
	expect(user, "POST", "/groups", `[{"kind":"tag","slug":"x"}] {}`, 400, "multiple JSON values")
	expect("", "GET", "/groups?kind=tag&language=en", "", 200, `"name":"Color"`)
	expect("", "GET", "/groups?sort=random", "", 400, "sort")
	expect("", "GET", "/groups/"+color, "", 200, `"names":[{"language":"en","kind":"name","name":"Color"}]`)
	expect("", "GET", "/groups/"+cid(99), "", 404, "not found")
	expect("", "GET", "/groups/not-an-id", "", 400, "group id")
	expect(user, "PATCH", "/groups/"+color, `{"slug":"colour","attributes":{"x":1}}`, 400, "not declared")
	expect(user, "PATCH", "/groups/"+color, `{"slug":"colour","text":[{"language":"en","description":"Hues"}]}`, 200, `"slug":"colour"`)
	expect(user, "POST", "/groups/"+color+"/names", `[{"language":"en","kind":"alias","name":"Hue"}]`, 200, `"name":"Hue"`)
	expect(user, "POST", "/groups/"+cid(99)+"/names", `[{"language":"en","name":"x"}]`, 404, "")
	expect(user, "POST", "/groups/"+color+"/edges", `[{"relation":"related","to":"`+saga+`"}]`, 200, `"relation":"related"`)
	expect(user, "POST", "/groups/"+color+"/edges", `[{"relation":"Bad Rel","to":"`+saga+`"}]`, 400, "relation")
	expect(user, "POST", "/groups/"+color+"/edges", `[{"relation":"related","to":"`+cid(99)+`"}]`, 404, "")
	expect(user, "PUT", "/contents/"+cid(1)+"/groups", `{"memberships":[{"group_id":"`+color+`"},{"group_id":"`+saga+`","position":3}]}`, 200, `"position":3`)
	expect(user, "PUT", "/contents/"+cid(1)+"/groups", `{"memberships":[{"group_id":"`+color+`","position":1}]}`, 400, "ordered")
	expect(user, "PUT", "/contents/"+cid(9)+"/groups", `{"memberships":[]}`, 404, "")
	expect(user, "GET", "/contents/"+cid(1)+"/groups", "", 200, `"groups":[`)
	expect(user, "POST", "/groups/"+saga+"/members", `[{"group_id":"`+color+`","content_id":"`+cid(1)+`"}]`, 400, "names group")
	expect(user, "PUT", "/groups/"+saga+"/members/order", `{"members":[{"content_id":"`+cid(1)+`"}]}`, 200, `"count":1`)
	expect(user, "GET", "/groups/"+saga+"/members?cursor=%21", "", 400, "cursor")
	expect(user, "POST", "/groups/"+color+"/merge", `{"into":"`+saga+`"}`, 409, "cannot merge")
	expect(user, "DELETE", "/groups/"+color, "", 200, `"state":"deleted"`)
	expect("", "GET", "/groups/"+color, "", 404, "")

	none, err := f.s.Handler(testauth.Auth{Authz: testauth.None{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/groups", strings.NewReader(`[{"kind":"tag","slug":"site"}]`))
	req.Header.Set("Authorization", "Bearer "+user)
	none.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without the permission: %d %s", rec.Code, rec.Body)
	}
}
