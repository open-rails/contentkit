package content

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/testauth"
)

func decodeErr(t *testing.T, body string) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return e
}

func TestErrorMapping_PollIDs(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{})
	h := rt.Handler()
	ctx := context.Background()
	admin := rt.as(access.Actor{ID: "admin"})
	poll, err := rt.polls.create(ctx, admin, twoOptionPoll("en"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, method, path string
		body               any
		status             int
		code               string
	}{
		{"malformed poll update", "PATCH", "/polls/not-a-uuid", PollUpdate{Question: ptr("changed")}, http.StatusNotFound, CodeNotFound},
		{"malformed vote option", "POST", "/polls/" + poll.ID + "/vote", map[string]string{"option_id": "not-a-uuid"}, http.StatusBadRequest, CodeInvalidRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, h, admin, c.method, c.path, c.body)
			got := decodeErr(t, rec.Body.String())
			if rec.Code != c.status || got.Code != c.code {
				t.Errorf("got %d %s; want %d %s", rec.Code, rec.Body.String(), c.status, c.code)
			}
		})
	}
}

// A host that never wired Media must get 501 not_configured on every image
// route, not a 500 indistinguishable from a crash.
func TestErrorMapping_UnwiredMediaIs501(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{})
	admin := rt.as(access.Actor{ID: "admin"})
	poll, err := rt.polls.create(context.Background(), admin, PollInput{Question: "Q?", Options: []PollOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatalf("create poll: %v", err)
	}
	name := image("i-00000000-0000-4000-8000-000000000000")
	for _, route := range []string{"PUT /polls/" + poll.ID + "/image", "PUT /polls/" + poll.ID + "/options/" + poll.Options[0].ID + "/image"} {
		method, path, _ := strings.Cut(route, " ")
		b, _ := json.Marshal(name)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req = testauth.As(req, admin)
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s -> %d %s, want 501", route, rec.Code, rec.Body.String())
		}
		got := decodeErr(t, rec.Body.String())
		if got.Code != CodeNotConfigured {
			t.Fatalf("%s -> code %q, want %q", route, got.Code, CodeNotConfigured)
		}
		if strings.Contains(got.Error, "content:") {
			t.Fatalf("%s leaks the internal error text: %q", route, got.Error)
		}
	}
}

// A resolver answering with another tenant's reference is a host
// misconfiguration: sanitized 500, distinguishable by code, cause in the log.
func TestErrorMapping_ForeignTenantRefIsSanitized500(t *testing.T) {
	var logs bytes.Buffer
	rt, _ := newTestRuntime(t, Options{
		Resolver: foreignResolver{},
		Kinds:    testKinds("gallery"),
		Logger:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	storeTarget(t, rt, "gallery", cid(1))

	req := httptest.NewRequest("GET", "/contents/"+cid(1)+"/comments", nil)
	req = testauth.As(req, access.Actor{ID: "u1"})
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d %s, want 500", rec.Code, rec.Body.String())
	}
	got := decodeErr(t, rec.Body.String())
	if got.Code != CodeTenantMismatch {
		t.Fatalf("code %q, want %q", got.Code, CodeTenantMismatch)
	}
	if got.Error != "internal error" {
		t.Fatalf("message %q must be sanitized", got.Error)
	}
	if strings.Contains(rec.Body.String(), "other") || strings.Contains(rec.Body.String(), "another tenant") {
		t.Fatalf("the cause reached the wire: %s", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, ErrTenant.Error()) {
		t.Fatalf("want the cause logged at ERROR, got: %s", out)
	}
}

// Every mounted route answers the same flat {error, code} body, and no 4xx
// carries an internal cause.
func TestErrorMapping_PublicCodes(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	res.set("gallery", cid(902), false, false)
	res.set("gallery", cid(901), true, false)
	var logs bytes.Buffer
	rt, _ := newTestRuntime(t, Options{
		Resolver: res,
		Auth:     testauth.Auth{Authz: denyAll{}},
		Kinds:    testKinds("gallery"),
		Logger:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	if _, err := rt.store.pool.Exec(t.Context(), `INSERT INTO `+rt.store.t.items+` (tenant_id, id, kind) VALUES ($1, $2, 'widget')`, rt.tenant, cid(78)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"unknown target", "GET", "/contents/" + cid(77) + "/comments", "", http.StatusNotFound, CodeNotFound},
		{"unregistered kind", "GET", "/contents/" + cid(78) + "/comments", "", http.StatusNotFound, CodeNotFound},
		{"not visible", "GET", "/contents/" + cid(902) + "/comments", "", http.StatusNotFound, CodeNotFound},
		{"not accessible", "POST", "/contents/" + cid(901) + "/comments", `{"body":"hi"}`, http.StatusForbidden, CodeForbidden},
		{"invalid body", "POST", "/contents/" + cid(1) + "/comments", `{"nope":1}`, http.StatusBadRequest, CodeInvalidRequest},
		{"denied perm", "POST", "/polls", `{"question":"q"}`, http.StatusForbidden, CodeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req = testauth.As(req, access.Actor{ID: "u1"})
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%s %s -> %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body.String(), tc.status)
			}
			got := decodeErr(t, rec.Body.String())
			if got.Code != tc.code {
				t.Fatalf("%s %s -> code %q, want %q", tc.method, tc.path, got.Code, tc.code)
			}
			if got.Error == "" {
				t.Fatalf("%s %s -> empty message", tc.method, tc.path)
			}
		})
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a 4xx logged at ERROR: %s", logs.String())
	}
}

// A moderator rejection stays a 422 with the author-facing reason and the
// stable moderation code.
func TestErrorMapping_ModerationRejection(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, Kinds: testKinds("gallery"), Moderator: &fakeModerator{}})

	req := httptest.NewRequest("POST", "/contents/"+cid(1)+"/comments", strings.NewReader(`{"body":"spam"}`))
	req = testauth.As(req, access.Actor{ID: "u1"})
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body.String())
	}
	got := decodeErr(t, rec.Body.String())
	if got.Code != CodeModerationRejected || got.Error != "spam is not allowed" {
		t.Fatalf("got %+v, want %s / %q", got, CodeModerationRejected, "spam is not allowed")
	}
}
