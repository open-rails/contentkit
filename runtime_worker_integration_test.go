package contentkit

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

func TestWorkerOptionsMissingHostLister(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	schema := keywordSchema(t, ctx, pool)
	rt, err := NewRuntime(ctx, RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: testTenant},
		Content: content.Options{Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: galleryResolver{},
			ContentKinds: []string{"video"}, Perms: content.Perms{PostWrite: "post:write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	post := do(t, rt.Handler(), access.Actor{ID: "u1", Kind: "user"}, http.MethodPost, "/posts", map[string]any{
		"title": "Worker Post", "body": "body", "language": "en", "is_draft": false,
	})
	if post.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", post.Code, post.Body.String())
	}
	buildHost := func(_ context.Context, _, _, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		docs := make([]search.KeywordDocument, 0, len(refs))
		for _, ref := range refs {
			docs = append(docs, search.KeywordDocument{
				DocumentKey: search.DocumentKey{ContentRef: ref, Language: language}, Title: "Worker Video",
			})
		}
		return docs, nil
	}
	missing := rt.WorkerOptions(worker.Options{
		SupportedLanguages: []string{"en"}, ContentKinds: []string{"video"}, BuildKeywordDocuments: buildHost,
	})
	postRefs, _, done, err := missing.ListContent(ctx, testTenant, content.KindPost, "en", "", 100)
	if err != nil || !done || len(postRefs) != 1 {
		t.Fatalf("post listing without host lister: refs=%d done=%t err=%v", len(postRefs), done, err)
	}
	err = worker.SyncOnce(ctx, missing)
	var state, cursor, lastError string
	if scanErr := pool.QueryRow(ctx, fmt.Sprintf(`SELECT state, cursor, COALESCE(last_error, '') FROM %s.content_search_backfill WHERE tenant_id=$1 AND content_kind='video' AND language='en'`, schema), testTenant).Scan(&state, &cursor, &lastError); scanErr != nil {
		t.Fatal(scanErr)
	}
	if err == nil || state != "failed" || cursor != "" || !strings.Contains(lastError, "ListContent") {
		t.Fatalf("missing lister: err=%v state=%q cursor=%q last_error=%q", err, state, cursor, lastError)
	}
	var postTitle string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT title FROM %s.content_search_documents WHERE tenant_id=$1 AND content_kind='post' AND content_id=$2 AND language='en'`, schema), testTenant, postRefs[0].ContentID).Scan(&postTitle); err != nil || postTitle != "Worker Post" {
		t.Fatalf("post indexed without host lister: title=%q err=%v", postTitle, err)
	}

	recovered := rt.WorkerOptions(worker.Options{
		SupportedLanguages: []string{"en"}, ContentKinds: []string{"video"},
		ListContent: func(_ context.Context, tenant, kind, _, _ string, _ int) ([]contentref.ContentRef, string, bool, error) {
			return []contentref.ContentRef{contentref.New(tenant, kind, cid(2))}, "", true, nil
		},
		BuildKeywordDocuments: buildHost,
	})
	for i := 0; i < 2; i++ {
		if err := worker.SyncOnce(ctx, recovered); err != nil {
			t.Fatalf("recovered tick %d: %v", i, err)
		}
	}
	var title string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT title FROM %s.content_search_documents WHERE tenant_id=$1 AND content_kind='video' AND content_id=$2 AND language='en'`, schema), testTenant, cid(2)).Scan(&title); err != nil || title != "Worker Video" {
		t.Fatalf("recovered video: title=%q err=%v", title, err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT state, cursor, COALESCE(last_error, '') FROM %s.content_search_backfill WHERE tenant_id=$1 AND content_kind='video' AND language='en'`, schema), testTenant).Scan(&state, &cursor, &lastError); err != nil || state != "done" || cursor != "" || lastError != "" {
		t.Fatalf("recovered backfill: state=%q cursor=%q last_error=%q err=%v", state, cursor, lastError, err)
	}
}
