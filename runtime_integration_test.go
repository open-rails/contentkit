package contentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit/chmigrate"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
	"github.com/open-rails/contentkit/worker"
)

type actorKey struct{}

type ctxIdentity struct{}

func (ctxIdentity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(access.Actor)
	return a, ok
}

type allowAuthz struct{}

func (allowAuthz) Can(context.Context, access.Actor, string) (bool, error) { return true, nil }

// galleryResolver knows only gallery cid(1), under its id or any suffixed alias ("<id>:en").
type galleryResolver struct{}

func (galleryResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if r.ContentKind == "gallery" && strings.HasPrefix(r.ContentID, cid(1)) {
			out[r.Key()] = access.Resolution{Ref: contentref.New(r.TenantID, "gallery", cid(1)), Visible: true, Accessible: true}
		}
	}
	return out, nil
}

func do(t *testing.T, h http.Handler, actor access.Actor, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req = req.WithContext(context.WithValue(req.Context(), actorKey{}, actor))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// One migrate call, one constructor: comments, reactions, favorites, polls,
// posts, keyword search (host galleries + ContentKit posts through one worker)
// and the signal plane, on real Postgres and ClickHouse.
func TestRuntimeIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	env := signaltest.FromEnv(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	hostSchema := pgtest.EmptySchema(t, ctx, pool)
	searchSchema := hostSchema
	unique := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	chDB, app := "ck_rt_"+unique, "ck_test_signal_"+unique
	conn := env.Empty(t, chDB)
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+searchSchema+" CASCADE")
		_, _ = pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE schema = $1 OR app = $2", searchSchema, app)
	})
	cfg := MigrateConfig{DB: sqlDB, Schema: hostSchema, ClickHouse: &chmigrate.Config{
		ClientAddr: env.Addr, Database: chDB, Username: env.User, Password: env.Password, App: app, Cluster: env.Cluster,
	}}
	for i := 0; i < 2; i++ { // idempotent
		if err := Migrate(ctx, cfg); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
	if err := signal.CheckSchema(ctx, conn, chDB); err != nil {
		t.Fatalf("signal schema after Migrate: %v", err)
	}

	rt, err := NewRuntime(ctx, RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: searchSchema, Tenant: "doujins", CH: conn, CHDatabase: chDB},
		Content: content.Options{
			Schema: hostSchema, Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: galleryResolver{},
			ContentKinds: []string{"gallery"}, Perms: content.Perms{PostWrite: "post:write", PollWrite: "poll:write"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(ctx, RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: searchSchema, Tenant: "doujins"},
		Content:        content.Options{Schema: hostSchema, Tenant: "hentai0", Identity: ctxIdentity{}, Authz: allowAuthz{}, Resolver: galleryResolver{}},
	}); err == nil {
		t.Fatal("a content tenant differing from the hub tenant was accepted")
	}
	h := rt.Handler()
	user := access.Actor{ID: "u1", Kind: "user", IP: "10.0.0.1"}

	// Interactions over one HTTP mount, keyed by the resolver's canonical reference.
	if rec := do(t, h, user, "POST", galleryRoute(1, ":en/comments"), map[string]string{"body": "first"}); rec.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, user, "POST", galleryRoute(1, "/like"), nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"likes":1`) {
		t.Fatalf("like: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, user, "POST", galleryRoute(1, "/favorite"), nil); rec.Code != http.StatusOK {
		t.Fatalf("favorite: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, user, "POST", "/video/"+cid(1)+"/like", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered kind: %d, want 404", rec.Code)
	}
	if rec := do(t, h, access.Actor{Anonymous: true, IP: "10.0.0.2"}, "POST", galleryRoute(1, "/favorite"), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous favorite: %d, want 401", rec.Code)
	}
	g1 := rt.Content.Ref("gallery", cid(1))
	counts, err := rt.Content.Counts(ctx, []contentref.ContentRef{g1})
	if err != nil || counts[g1.Key()] != (content.Counts{Likes: 1, Favorites: 1, CommentCount: 1}) {
		t.Fatalf("counts = %+v err=%v", counts, err)
	}
	if _, err := rt.Content.Counts(ctx, []contentref.ContentRef{contentref.New("hentai0", "gallery", cid(1))}); !errors.Is(err, content.ErrTenant) {
		t.Fatalf("foreign tenant: want ErrTenant, got %v", err)
	}
	rec := do(t, h, user, "POST", "/polls", map[string]any{"question": "Best?", "options": []map[string]any{{"label": "a"}, {"label": "b"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	var poll struct {
		ID      string `json:"id"`
		Options []struct {
			ID string `json:"id"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, user, "POST", "/polls/"+poll.ID+"/vote", map[string]string{"option_id": poll.Options[0].ID}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total_votes":1`) {
		t.Fatalf("vote: %d %s", rec.Code, rec.Body.String())
	}

	// A post is a search document: its write queues it, the worker builds it.
	rec = do(t, h, user, "POST", "/posts", map[string]any{"title": "Autumn Festival Report", "body": "b", "language": "en", "is_draft": false})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	opts := rt.WorkerOptions(worker.Options{
		SupportedLanguages: []string{"en"}, ContentKinds: []string{"gallery"},
		ListContent: func(_ context.Context, tenant, kind, _, _ string, _ int) ([]contentref.ContentRef, string, bool, error) {
			return []contentref.ContentRef{contentref.New(tenant, kind, cid(1))}, "", true, nil
		},
		BuildKeywordDocuments: func(_ context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
			var docs []search.KeywordDocument
			for _, r := range refs {
				docs = append(docs, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: r, Language: language}, Title: "Moonlit Garden"})
			}
			return docs, nil
		},
	})
	for i := 0; i < 3; i++ { // backfill queues, next ticks build
		if err := worker.SyncOnce(ctx, opts); err != nil {
			t.Fatalf("worker tick %d: %v", i, err)
		}
	}
	for _, c := range []struct{ q, kind, want string }{{"autumn festival", content.KindPost, "Autumn"}, {"moonlit", "gallery", cid(1)}} {
		res, err := rt.Search(ctx, c.q, HubSearchOptions{SearchOptions: SearchOptions{Language: "en", ContentKinds: []string{c.kind}}})
		if err != nil || len(res.Hits) != 1 || res.Hits[0].ContentKind != c.kind {
			t.Fatalf("search %q: %+v err=%v", c.q, res, err)
		}
	}

	// The signal plane runs on the same hub.
	if err := rt.RecordSignals(ctx, []signal.Signal{{ContentRef: g1, Subject: signal.Subject{UserID: "u1"}, Type: signal.TypeView, EventID: "s1", OccurredAt: time.Now().UTC(), Progress: 10, ProgressMax: 10}}); err != nil {
		t.Fatal(err)
	}
	top, err := rt.Popular(ctx, "gallery", signal.PopularOptions{Window: signal.LastDays(7, time.Now())})
	if err != nil || len(top) != 1 || top[0].ContentID != cid(1) {
		t.Fatalf("popular = %+v err=%v", top, err)
	}
}
