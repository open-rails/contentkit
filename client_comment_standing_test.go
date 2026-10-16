package contentkit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ck "github.com/open-rails/contentkit"
)

func TestClientCommentStandingLanguageAndOptionalCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/contents/story/can-comment":
			if r.URL.Query().Get("language") != "ja" || r.URL.Query().Get("version") != "edition" {
				t.Errorf("thread query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"count":23}`))
		case "/v1/entities/tag/can-comment":
			if r.URL.Query().Get("language") != "en" {
				t.Errorf("group query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"count":0}`))
		case "/v1/contents/locked/can-comment":
			_, _ = w.Write([]byte(`{"locked":true}`))
		default:
			t.Errorf("path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := ck.NewClient(server.URL, nil)
	thread, err := client.GetContentCanComment(context.Background(), "story", ck.GetContentCanCommentQuery{Version: "edition", Language: "ja"})
	if err != nil || thread.Count == nil || *thread.Count != 23 {
		t.Fatalf("thread count: %+v %v", thread, err)
	}
	group, err := client.GetEntityCanComment(context.Background(), "tag", ck.GetEntityCanCommentQuery{Language: "en"})
	if err != nil || group.Count == nil || *group.Count != 0 {
		t.Fatalf("empty count: %+v %v", group, err)
	}
	locked, err := client.GetContentCanComment(context.Background(), "locked", ck.GetContentCanCommentQuery{})
	if err != nil || locked.Count != nil || !locked.Locked {
		t.Fatalf("withheld count: %+v %v", locked, err)
	}
}
