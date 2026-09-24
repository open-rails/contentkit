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
)

func decodeErr(t *testing.T, body string) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return e
}

// A host that never wired Media must get 501 not_configured on every image
// route, not a 500 indistinguishable from a crash.
func TestErrorMapping_UnwiredMediaIs501(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Perms: Perms{PostWrite: "post", PollWrite: "poll"}})
	admin := access.Actor{ID: "admin"}
	poll, err := rt.polls.create(context.Background(), admin, createPollInput{Question: "Q?", Options: []createOptionInput{{Label: "A"}, {Label: "B"}}})
	if err != nil {
		t.Fatalf("create poll: %v", err)
	}
	post := insertPost(t, rt)
	name := image("i-00000000-0000-4000-8000-000000000000")
	for _, route := range []string{"PUT /posts/" + post + "/cover", "POST /posts/" + post + "/images", "PUT /polls/" + poll.ID + "/image",
		"PUT /polls/" + poll.ID + "/options/" + poll.Options[0].ID + "/image"} {
		method, path, _ := strings.Cut(route, " ")
		b, _ := json.Marshal(name)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req = req.WithContext(withActor(req.Context(), admin))
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
		Resolver:     foreignResolver{},
		ContentKinds: []string{"gallery"},
		Logger:       slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	req := httptest.NewRequest("GET", "/gallery/g1/comments", nil)
	req = req.WithContext(withActor(req.Context(), access.Actor{ID: "u1"}))
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
	res.set("gallery", "g1", true, true)
	res.set("gallery", "hidden", false, false)
	res.set("gallery", "locked", true, false)
	var logs bytes.Buffer
	rt, _ := newTestRuntime(t, Options{
		Resolver:     res,
		Authz:        denyAll{},
		ContentKinds: []string{"gallery"},
		Perms:        Perms{PostWrite: "post", PollWrite: "poll"},
		Logger:       slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"unregistered kind", "GET", "/widget/w1/comments", "", http.StatusNotFound, CodeNotFound},
		{"not visible", "GET", "/gallery/hidden/comments", "", http.StatusNotFound, CodeNotFound},
		{"not accessible", "POST", "/gallery/locked/comments", `{"body":"hi"}`, http.StatusForbidden, CodeForbidden},
		{"invalid body", "POST", "/gallery/g1/comments", `{"nope":1}`, http.StatusBadRequest, CodeInvalidRequest},
		{"denied perm", "POST", "/posts", `{"title":"t","body":"b"}`, http.StatusForbidden, CodeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req = req.WithContext(withActor(req.Context(), access.Actor{ID: "u1"}))
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
	res.set("gallery", "g1", true, true)
	rt, _ := newTestRuntime(t, Options{Resolver: res, ContentKinds: []string{"gallery"}, Moderator: &fakeModerator{}})

	req := httptest.NewRequest("POST", "/gallery/g1/comments", strings.NewReader(`{"body":"spam"}`))
	req = req.WithContext(withActor(req.Context(), access.Actor{ID: "u1"}))
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
