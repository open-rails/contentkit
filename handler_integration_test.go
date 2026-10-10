package contentkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/taxonomy"
)

const mountPrefix = "/api/contentkit"

// staffAuthz grants every permission to the actor "staff" and none to anyone else.
type staffAuthz struct{}

func (staffAuthz) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff" && !a.Anonymous, nil
}

// itemResolver shows gallery cid(1), owned by "owner", to everyone, and lets
// signed-in actors edit its media.
type itemResolver struct{}

func (itemResolver) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if r.ContentKind == "gallery" && r.ContentID == cid(1) {
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true, Owner: "owner", Editor: !a.Anonymous && a.ID != ""}
		}
	}
	return out, nil
}

type uploadAllow struct{}

func (uploadAllow) CanUpload(_ context.Context, a access.Actor, _ media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: !a.Anonymous && a.ID != ""}, nil
}

// mountFixture is Runtime.Handler mounted once under mountPrefix behind a
// stand-in auth middleware, every response held to the route catalog.
type mountFixture struct {
	t       *testing.T
	h       http.Handler
	checker *contract.Checker
	media   bool

	mu     sync.Mutex
	broken []string
}

func (f *mountFixture) call(actor access.Actor, method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, mountPrefix+target, &buf)
	req = req.WithContext(context.WithValue(req.Context(), actorKey{}, actor))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// want calls and requires status, decoding a JSON body into out when given.
func (f *mountFixture) want(status int, actor access.Actor, method, target string, body, out any) {
	f.t.Helper()
	rec := f.call(actor, method, target, body)
	if rec.Code != status {
		f.t.Fatalf("%s %s: %d, want %d: %s", method, target, rec.Code, status, rec.Body)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("%s %s: %v", method, target, err)
		}
	}
}

func newMountFixture(t *testing.T) *mountFixture {
	ctx := context.Background()
	pool := testPG(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, MigrateConfig{DB: db, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	const tenant = "site"
	urls, err := contenturl.New(contenturl.Options{Pool: pool, Schema: schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	var rt *Runtime
	router, err := contenturl.NewRouter(urls, contenturl.RouterOptions{
		Routes: contenturl.Routes{"post": "blog", "tag": "tag"}, Languages: []string{"en"},
		Visibility: func(r *http.Request, l contenturl.Link) (contenturl.Visibility, error) {
			if l.ContentKind == content.KindPost {
				return rt.Content.PostVisibility(r.Context(), l.ContentID)
			}
			return contenturl.Visible, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	tax, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: []string{"tag"}, Languages: []string{"en"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := RuntimeConfig{
		EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: tenant},
		Content: content.Options{Identity: ctxIdentity{}, Authz: staffAuthz{}, Resolver: itemResolver{}, ContentKinds: []string{"gallery"},
			Limits: content.Limits{Disabled: true},
			Perms: content.Perms{PostWrite: "post", PollWrite: "poll", CommentModerate: "moderate", ModerationReview: "review",
				CommentBan: "ban", Taxonomy: "taxonomy"}},
		Codes:     router,
		Taxonomy:  taxonomy.Handler(tax),
		ReadLimit: media.RateLimit{Disabled: true},
	}
	f := &mountFixture{t: t}
	if os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT") != "" {
		cfg.Uploads, cfg.Reader = mediaStack(t)
		f.media = true
	}
	if rt, err = NewRuntime(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if f.checker, err = contract.NewChecker(os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(mountPrefix+"/", http.StripPrefix(mountPrefix, rt.Handler()))
	f.h = f.checker.Wrap(mountPrefix, mux, func(err error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.broken = append(f.broken, err.Error())
	})
	t.Cleanup(func() {
		for _, b := range f.broken {
			t.Errorf("contract: %s", b)
		}
	})
	return f
}

func mediaStack(t *testing.T) (*media.Uploads, *media.Reader) {
	env := mediatest.Open(t)
	images := []string{"image/png"}
	reg, err := media.NewRegistry(media.Config{Namespace: env.Tenant, BaseURL: "https://media.test",
		Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "originals/{name}", Types: images, MaxBytes: 1 << 20, Max: 10}, {Path: "cover", Types: images, MaxBytes: 1 << 20},
				{Path: "extras/{name}", Types: images, MaxBytes: 1 << 20, Max: 1}},
			Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{200}, Image: media.Image{MinWidth: 8}}}}},
		Hooks: media.Hooks{Resolver: itemResolver{}, CanUpload: uploadAllow{}}})
	if err != nil {
		t.Fatal(err)
	}
	journal, queue := env.Processing(reg)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: mediatest.Locker(t, env.Store), Journal: journal, Processes: queue, Pool: env.Pool()})
	if err != nil {
		t.Fatal(err)
	}
	key := token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}
	ring, err := token.NewRing(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: jobs.Manifests(), Tickets: &ring, Commits: media.RateLimit{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := media.NewReader(media.ReaderOptions{Manifests: jobs.Manifests(), Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: key}})
	if err != nil {
		t.Fatal(err)
	}
	return up, rd
}

// One mount serves every module at its sub-path, and every route answers
// what the catalog declares: each response of this scenario is checked
// against its route, and every route of the catalog is reached.
func TestHandlerServesTheCatalogIntegration(t *testing.T) {
	f := newMountFixture(t)
	staff := access.Actor{ID: "staff", Kind: "user"}
	owner := access.Actor{ID: "owner", Kind: "user"}
	user := access.Actor{ID: "u1", Kind: "user", IP: "10.0.0.1"}
	anon := access.Actor{Anonymous: true, IP: "10.0.0.9"}
	item := "/gallery/" + cid(1)

	// Posts.
	var post content.Post
	f.want(http.StatusForbidden, user, "POST", "/posts", content.PostInput{Title: ptr("Hello"), Body: ptr("World")}, nil)
	f.want(http.StatusCreated, staff, "POST", "/posts", content.PostInput{Title: ptr("Hello"), Body: ptr("World"), Language: ptr("en")}, &post)
	f.want(http.StatusOK, anon, "GET", "/posts?language=en&sort=likes", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/posts/admin?draft=false&language=en", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/posts/admin", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/posts/"+post.ID, nil, nil)
	f.want(http.StatusOK, staff, "PATCH", "/posts/"+post.ID, content.PostInput{Excerpt: ptr("Short")}, nil)
	for _, verb := range []string{"like", "dislike", "neutral"} {
		f.want(http.StatusOK, user, "POST", "/posts/"+post.ID+"/"+verb, nil, nil)
	}
	f.want(http.StatusNotImplemented, staff, "PUT", "/posts/"+post.ID+"/cover", content.ImageInput{Image: ptr("")}, nil)
	f.want(http.StatusNotImplemented, staff, "POST", "/posts/"+post.ID+"/images", content.ImageInput{Image: ptr("i-" + uuid.NewString())}, nil)

	// Codes resolve the post's code.
	var resolved contenturl.Resolved
	f.want(http.StatusOK, anon, "GET", "/codes/"+strings.ToLower(post.Code)+"?lang=en", nil, &resolved)
	if resolved.ContentID != post.ID || resolved.Path != "/en/blog/"+post.Code+"/"+post.URLSlug {
		t.Fatalf("resolved %+v", resolved)
	}
	f.want(http.StatusBadRequest, anon, "GET", "/codes/not-a-code", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/codes/ZZZZZZZZ9", nil, nil)
	f.want(http.StatusOK, staff, "DELETE", "/posts/"+post.ID, nil, nil)
	f.want(http.StatusGone, anon, "GET", "/codes/"+post.Code, nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/posts/"+post.ID, nil, nil)

	// Comments.
	var root, reply content.Comment
	f.want(http.StatusCreated, user, "POST", item+"/comments", content.CommentInput{Body: "first"}, &root)
	f.want(http.StatusBadRequest, anon, "POST", item+"/comments", content.CommentInput{Body: "who?"}, nil)
	f.want(http.StatusCreated, owner, "POST", item+"/comments", content.CommentInput{Body: "reply", ReplyToID: root.ID}, &reply)
	f.want(http.StatusOK, anon, "GET", item+"/comments?sort=best&limit=5", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/comments/"+root.ID+"/replies", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/comments/latest", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/comments/admin", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/comments/admin?content_kind=gallery", nil, nil)
	f.want(http.StatusOK, user, "PATCH", "/comments/"+root.ID, content.CommentEdit{Body: "first, edited"}, nil)
	f.want(http.StatusForbidden, owner, "PATCH", "/comments/"+root.ID, content.CommentEdit{Body: "not yours"}, nil)
	for _, verb := range []string{"like", "dislike", "neutral"} {
		f.want(http.StatusOK, owner, "POST", "/comments/"+root.ID+"/"+verb, nil, nil)
	}
	f.want(http.StatusNoContent, owner, "DELETE", "/comments/"+reply.ID, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/comments/"+reply.ID+"/restore", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/gallery/"+cid(2)+"/comments", nil, nil)

	// Reactions on host content.
	for _, verb := range []string{"like", "dislike", "neutral"} {
		f.want(http.StatusOK, user, "POST", item+"/"+verb, nil, nil)
	}
	f.want(http.StatusOK, user, "DELETE", item+"/reaction", nil, nil)
	f.want(http.StatusOK, anon, "GET", item+"/reaction", nil, nil)

	// Favorites.
	f.want(http.StatusUnauthorized, anon, "POST", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "POST", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "GET", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "GET", "/favorites", nil, nil)
	f.want(http.StatusOK, user, "DELETE", item+"/favorite", nil, nil)

	// Polls.
	var poll content.Poll
	f.want(http.StatusCreated, staff, "POST", "/polls", content.PollInput{Question: "Best?", Language: "en",
		Options: []content.PollOptionInput{{Label: "A"}, {Label: "B", Position: 1}}}, &poll)
	f.want(http.StatusNotImplemented, staff, "POST", "/polls", content.PollInput{Kind: content.PollFreeText, Question: "Why?", Language: "en"}, nil)
	f.want(http.StatusOK, anon, "GET", "/polls?language=en", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/polls/admin", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/polls/"+poll.ID, nil, nil)
	f.want(http.StatusOK, user, "POST", "/polls/"+poll.ID+"/vote", content.PollVote{OptionID: poll.Options[0].ID}, nil)
	f.want(http.StatusUnauthorized, anon, "POST", "/polls/"+poll.ID+"/answer", content.PollAnswerInput{Text: "because"}, nil)
	f.want(http.StatusOK, staff, "PATCH", "/polls/"+poll.ID, content.PollUpdate{Question: ptr("Best one?")}, nil)
	var option content.PollOption
	f.want(http.StatusCreated, staff, "POST", "/polls/"+poll.ID+"/options", content.PollOptionPatch{Label: ptr("C")}, &option)
	f.want(http.StatusOK, staff, "PATCH", "/polls/"+poll.ID+"/options/"+option.ID, content.PollOptionPatch{Position: ptr(5)}, nil)
	f.want(http.StatusNotImplemented, staff, "PUT", "/polls/"+poll.ID+"/image", content.ImageInput{Image: ptr("")}, nil)
	f.want(http.StatusNotImplemented, staff, "PUT", "/polls/"+poll.ID+"/options/"+option.ID+"/image", content.ImageInput{Image: ptr("")}, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/polls/"+poll.ID+"/options/"+option.ID, nil, nil)
	f.want(http.StatusBadRequest, staff, "DELETE", "/polls/"+poll.ID+"/options/"+poll.Options[0].ID, nil, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/polls/"+poll.ID, nil, nil)

	// Bans: the owner's own scope, and the tenant-wide one.
	f.want(http.StatusOK, owner, "PUT", "/comment-bans/u1", content.BanInput{Reason: "spam"}, nil)
	f.want(http.StatusOK, owner, "GET", "/comment-bans", nil, nil)
	var standing content.CommentStanding
	f.want(http.StatusOK, user, "GET", item+"/can-comment", nil, &standing)
	if standing.CanComment || standing.Ban == nil {
		t.Fatalf("banned standing %+v", standing)
	}
	f.want(http.StatusForbidden, user, "POST", item+"/comments", content.CommentInput{Body: "again"}, nil)
	f.want(http.StatusNoContent, owner, "DELETE", "/comment-bans/u1", nil, nil)
	f.want(http.StatusUnauthorized, anon, "GET", "/comment-bans", nil, nil)
	f.want(http.StatusOK, staff, "PUT", "/global-comment-bans/u1", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/global-comment-bans", nil, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/global-comment-bans/u1", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/global-comment-bans", nil, nil)

	// Moderation.
	f.want(http.StatusOK, staff, "GET", "/moderation/held?kind=comment", nil, nil)
	f.want(http.StatusBadRequest, staff, "GET", "/moderation/held?kind=gallery", nil, nil)
	f.want(http.StatusNotFound, staff, "POST", "/moderation/comment/"+uuid.NewString()+"/resolve", content.ResolveInput{Decision: content.DecisionApprove, Revision: 1}, nil)
	f.want(http.StatusForbidden, user, "GET", "/moderation/held?kind=comment", nil, nil)

	// Taxonomy, behind Perms.Taxonomy.
	f.want(http.StatusForbidden, user, "GET", "/taxonomy/nodes", nil, nil)
	var nodes []taxonomy.Node
	name := func(n string) taxonomy.Name {
		return taxonomy.Name{Language: "en", Kind: taxonomy.NameCanonical, Name: n}
	}
	f.want(http.StatusCreated, staff, "POST", "/taxonomy/nodes", []taxonomy.NodeInput{
		{Kind: "tag", Slug: "color", Names: []taxonomy.Name{name("Color")}},
		{Kind: "tag", Slug: "colour", Names: []taxonomy.Name{name("Colour")}},
		{Kind: "tag", Slug: "art", Names: []taxonomy.Name{name("Art")}},
	}, &nodes)
	color, colour, art := string(nodes[0].TaxonomyID), string(nodes[1].TaxonomyID), string(nodes[2].TaxonomyID)
	f.want(http.StatusConflict, staff, "POST", "/taxonomy/nodes", []taxonomy.NodeInput{{Kind: "tag", Slug: "color"}}, nil)
	f.want(http.StatusOK, staff, "GET", "/taxonomy/nodes?kind=tag&language=en&sort=name&limit=10", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/taxonomy/nodes/"+color, nil, nil)
	f.want(http.StatusNotFound, staff, "GET", "/taxonomy/nodes/"+uuid.Must(uuid.NewV7()).String(), nil, nil)
	f.want(http.StatusBadRequest, staff, "GET", "/taxonomy/nodes/not-an-id", nil, nil)
	f.want(http.StatusOK, staff, "PATCH", "/taxonomy/nodes/"+art, taxonomy.NodeUpdate{Slug: ptr("arts")}, nil)
	f.want(http.StatusOK, staff, "POST", "/taxonomy/nodes/"+color+"/names", []taxonomy.Name{{Language: "en", Kind: taxonomy.NameAlias, Name: "Hue"}}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/taxonomy/nodes/"+color+"/names", []taxonomy.Name{{Language: "en", Kind: taxonomy.NameAlias, Name: "Hue"}}, nil)
	f.want(http.StatusOK, staff, "PUT", "/taxonomy/nodes/"+color+"/names", []taxonomy.Name{name("Color")}, nil)
	edges := []taxonomy.Edge{{From: taxonomy.TaxonomyID(art), Relation: taxonomy.RelationParent, To: taxonomy.TaxonomyID(color)}}
	f.want(http.StatusOK, staff, "POST", "/taxonomy/edges", edges, nil)
	f.want(http.StatusOK, staff, "DELETE", "/taxonomy/edges", edges, nil)
	assignments := []taxonomy.Assignment{{ContentRef: contentref.ContentRef{ContentKind: "gallery", ContentID: cid(1)}, TaxonomyID: taxonomy.TaxonomyID(color), Relation: "tag"}}
	f.want(http.StatusOK, staff, "POST", "/taxonomy/assignments?suppress_counts=true", assignments, nil)
	f.want(http.StatusOK, staff, "POST", "/taxonomy/counts/rebuild", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/taxonomy/counts?taxonomy_id="+color, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/taxonomy/effective", []contentref.ContentRef{{ContentKind: "gallery", ContentID: cid(1)}}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/taxonomy/assignments", assignments, nil)
	f.want(http.StatusOK, staff, "POST", "/taxonomy/nodes/"+colour+"/merge", taxonomy.MergeInput{Into: taxonomy.TaxonomyID(color)}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/taxonomy/nodes/"+art, nil, nil)

	if f.media {
		mediaScenario(t, f, user, anon)
	}

	served := f.checker.Served()
	var missing []string
	for _, r := range httpapi.Catalog() {
		if (r.Module == httpapi.Upload || r.Module == httpapi.Media) && !f.media {
			continue
		}
		if !served[contract.RouteID(r)] {
			missing = append(missing, contract.RouteID(r))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes this scenario never reached:\n  %s", strings.Join(missing, "\n  "))
	}
}

func mediaScenario(t *testing.T, f *mountFixture, user, anon access.Actor) {
	ref := media.RefBody{Kind: "gallery", ID: cid(1)}
	body := pngBytes(t, 16)
	sum := sha256.Sum256(body)
	presign := func(path string) media.PresignBody {
		return media.PresignBody{Ref: ref, Path: path, Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	}
	// upload presigns path and PUTs the bytes where the plan says, as a browser does.
	upload := func(path string) media.PresignReply {
		var plan media.PresignReply
		f.want(http.StatusOK, user, "POST", "/media/upload/presign", presign(path), &plan)
		if plan.Put == nil {
			return plan // the folder already holds these bytes
		}
		req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, b)
		}
		return plan
	}
	commit := func(plan media.PresignReply) *httptest.ResponseRecorder {
		return f.call(user, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
	}
	f.want(http.StatusUnauthorized, anon, "POST", "/media/upload/presign", presign("originals/001"), nil)
	if rec := commit(upload("originals/001")); rec.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}
	// too_many_files says how many the path holds.
	if rec := commit(upload("extras/a")); rec.Code != http.StatusOK {
		t.Fatalf("commit extras/a: %d %s", rec.Code, rec.Body)
	}
	var refused media.ErrorReply
	rec := commit(upload("extras/b"))
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusConflict ||
		refused.Code != media.CodeTooManyFiles || refused.Details == nil || refused.Details.Max != 1 {
		t.Fatalf("second extra: %d %s", rec.Code, rec.Body)
	}
	f.want(http.StatusUnsupportedMediaType, user, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: "cover", Type: "image/gif", Size: 10, SHA256: hex.EncodeToString(sum[:])}, nil)
	for _, p := range []string{"/media/upload/parts/list", "/media/upload/complete", "/media/upload/abort"} {
		f.want(http.StatusBadRequest, user, "POST", p, media.TicketBody{Ticket: "forged"}, nil)
	}
	f.want(http.StatusBadRequest, user, "POST", "/media/upload/parts", media.PartsBody{Ticket: "forged", Parts: []media.PartBody{{Number: 1, Size: 1, SHA256: hex.EncodeToString(sum[:])}}}, nil)
	f.want(http.StatusNotFound, user, "GET", "/media/upload/frame?kind=gallery&id="+cid(1)+"&path=originals/001&t=1", nil, nil)

	// An editor read carries the kind's upload rules; a viewer's does not.
	var read media.ReadResult
	f.want(http.StatusOK, user, "GET", "/media/gallery/"+cid(1)+"?editor", nil, &read)
	if len(read.Files) != 2 || !read.Files[0].Upload || len(read.Uploads) != 3 {
		t.Fatalf("editor read %+v", read)
	}
	if r := read.Uploads[0]; r.Path != "originals/{name}" || r.MaxBytes != 1<<20 || r.Max != 10 || len(r.Types) != 1 || r.Video != nil {
		t.Fatalf("originals rule %+v", r)
	}
	if r := read.Uploads[1]; r.Path != "cover" || r.MinWidth != 8 {
		t.Fatalf("cover rule %+v", r)
	}
	var viewer media.ReadResult
	f.want(http.StatusOK, anon, "GET", "/media/gallery/"+cid(1)+"?limit=10&editor", nil, &viewer)
	if viewer.Uploads != nil {
		t.Fatalf("a viewer read the upload rules: %+v", viewer.Uploads)
	}
	f.want(http.StatusNotFound, anon, "GET", "/media/gallery/"+cid(2), nil, nil)
	f.want(http.StatusNotFound, user, "GET", "/media/gallery/"+cid(1)+"/hls/master.m3u8?audio=ja", nil, nil)

	// The preset rules, without a read.
	var presets []media.PresetRule
	f.want(http.StatusOK, anon, "GET", "/media/presets", nil, &presets)
	if len(presets) != 1 || presets[0].Kind != "gallery" || presets[0].Name != "cover" || presets[0].MinWidth != 8 ||
		presets[0].Base != "https://media.test" || presets[0].To != "cover-{w}.webp" || len(presets[0].Widths) != 1 {
		t.Fatalf("presets %+v", presets)
	}
}

func pngBytes(t *testing.T, w int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	for i := range w {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRuntimeRefusesReservedKindsIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	if err := Migrate(ctx, MigrateConfig{DB: db, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range httpapi.Reserved() {
		_, err := NewRuntime(ctx, RuntimeConfig{EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: "site"},
			Content: content.Options{Identity: ctxIdentity{}, Authz: staffAuthz{}, Resolver: itemResolver{}, ContentKinds: []string{kind}}})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", kind)) {
			t.Fatalf("content kind %q: %v", kind, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
