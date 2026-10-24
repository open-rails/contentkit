package authkit_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

const tenant = "onlydemo"

var site = kinds.Config{Languages: []string{"en"},
	Kinds: map[string]kinds.Kind{"post": {Route: "p", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true},
		Entities: []string{"channel", "tag"}}},
	EntityKinds: map[string]kinds.EntityKind{
		"channel": {Route: "c", Owner: "user", AuthScope: true, Entitled: true, MaxPerOwner: 2},
		"tag":     {Route: "tags"},
	}}

// world is a real AuthKit, whose app declares the channel persona with an
// editor role holding channel:settings:manage, and the groups store over
// ContentKit's schema with this adapter as its Scopes.
type world struct {
	t      *testing.T
	ak     *authkit.Client
	editor iam.Role
	writer iam.Role // channel:content:write only
	staff  iam.Role
	store  *entities.Store
	bare   *entities.Store // the same tables without Scopes
	srv    *httptest.Server
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if os.Getenv("CONTENTKIT_TEST_URL") == "" && os.Getenv("CONTENTKIT_TEST_REQUIRE_DB") == "1" {
		t.Fatal("CONTENTKIT_TEST_URL is required")
	}
	w := &world{t: t}
	rbac := authkit.NewRoles()
	var grants []iam.Grant
	for _, p := range rbac.Root.Declare(access.Names(access.Permissions()...)...) {
		grants = append(grants, p)
	}
	w.staff = rbac.Root.Role("staff", grants...)
	channel := rbac.Persona("channel")
	perms := channel.Declare(access.EntityPermissions("channel")...)
	settings := perms[slices.Index(access.EntityPermissions("channel"), access.EntitySettingsManage("channel"))]
	w.editor = channel.Role("editor", settings)
	w.writer = channel.Role("writer", perms[slices.Index(access.EntityPermissions("channel"), access.EntityContentWrite("channel"))])
	w.ak, _ = authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))

	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	var err error
	if w.store, err = entities.New(entities.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: site, Scopes: ckauthkit.Scopes(w.ak)}); err != nil {
		t.Fatal(err)
	}
	if w.bare, err = entities.New(entities.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: site}); err != nil {
		t.Fatal(err)
	}
	h, err := w.store.Handler(w.ak.Authenticator())
	if err != nil {
		t.Fatal(err)
	}
	w.srv = httptest.NewServer(h)
	t.Cleanup(w.srv.Close)
	return w
}

// person is a signed-in account and its access token.
func (w *world) person() (authtest.User, string) {
	u := authtest.NewUser(w.t, w.ak)
	return u, authtest.SignIn(w.t, w.ak, u).AccessToken
}

// call sends body as bearer and decodes the reply into out on success.
func (w *world) call(status int, bearer, method, path string, body, out any) {
	w.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, w.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		w.t.Fatalf("%s %s: %d %s, want %d", method, path, res.StatusCode, got, status)
	}
	if out != nil {
		if err := json.Unmarshal(got, out); err != nil {
			w.t.Fatal(err)
		}
	}
}

// scope is the AuthKit group of id: live, or deleted, and its owners.
func (w *world) scope(id string, people ...authtest.User) (iam.Group, map[iam.Subject]iam.Role) {
	w.t.Helper()
	g, err := w.ak.Group(w.t.Context(), iam.GroupByID(id))
	if err != nil {
		w.t.Fatal(err)
	}
	if len(people) == 0 {
		return g, nil
	}
	subjects := make([]iam.Subject, len(people))
	for i, p := range people {
		subjects[i] = iam.UserSubject(p.ID)
	}
	roles, err := w.ak.GroupRoles(w.t.Context(), iam.GroupByID(id), subjects)
	if err != nil {
		w.t.Fatal(err)
	}
	return g, roles
}

func name(n string) []entities.Name {
	return []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: n}}
}

// A person creates channels over a real AuthKit, which provisions each
// one's scope with them as owner; holders of channel:settings:manage there
// edit it and nobody else does; deleting retires the scope, restoring
// provisions it anew; the cap holds for creates and restores.
func TestCreatorWritesOverAuthKit(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	creator, creatorToken := w.person()
	editor, editorToken := w.person()
	_, strangerToken := w.person()
	staffUser, staffToken := w.person()
	authtest.GrantRole(t, w.ak, iam.RootGroup(), iam.UserSubject(staffUser.ID), w.staff)

	var made []entities.Entity
	w.call(201, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "studio", Names: name("Studio")}}, &made)
	studio := made[0]
	if studio.OwnerUserID != creator.ID || studio.AuthScope != string(studio.ID) {
		t.Fatalf("created %+v", studio)
	}
	g, roles := w.scope(studio.AuthScope, creator)
	if g.DeletedAt != nil || g.Persona.String() != "channel" || roles[iam.UserSubject(creator.ID)] != g.Persona.OwnerRole() {
		t.Fatalf("scope %+v roles %v", g, roles)
	}
	w.call(403, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "tag", Slug: "mine"}}, nil)
	w.call(403, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "theirs", OwnerUserID: editor.ID}}, nil)
	w.call(403, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "scoped", AuthScope: studio.AuthScope}}, nil)
	w.call(201, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "second"}}, nil)
	var refusal struct{ Code string }
	w.call(409, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "third"}}, &refusal)
	if refusal.Code != "limit_reached" {
		t.Fatalf("the cap answered %q", refusal.Code)
	}
	w.call(201, staffToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "staffs", OwnerUserID: editor.ID}}, nil)

	path := "/entities/" + string(studio.ID)
	w.call(403, editorToken, "PATCH", path, entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "x"}}}, nil)
	authtest.GrantRole(t, w.ak, iam.GroupByID(studio.AuthScope), iam.UserSubject(editor.ID), w.editor)
	var edited entities.Entity
	w.call(200, editorToken, "PATCH", path, entities.EntityUpdate{Names: name("Studio Two"), Attributes: map[string]any{}}, &edited)
	w.call(403, editorToken, "PATCH", path, entities.EntityUpdate{Slug: ptr("renamed")}, nil)
	var mine entities.MyEntityPage
	w.call(200, editorToken, "GET", "/me/entities?kind=channel", nil, &mine)
	if len(mine.Entities) != 2 || !mine.Entities[0].Editor && !mine.Entities[1].Editor {
		t.Fatalf("the editor's groups %+v", mine)
	}
	for _, g := range mine.Entities {
		// The editor owns the other, its scope's owner role included.
		if g.ID == studio.ID && (!g.Editor || g.Owner) || g.ID != studio.ID && (!g.Owner || !g.Editor) {
			t.Fatalf("the editor's %s: %+v", g.Slug, g)
		}
	}
	w.call(200, creatorToken, "GET", "/me/entities", nil, &mine)
	if len(mine.Entities) != 2 || !mine.Entities[0].Owner || !mine.Entities[1].Owner || !mine.Entities[0].Writer {
		t.Fatalf("the creator's groups %+v", mine)
	}
	// A holder of channel:content:write alone finds the channel as its writer.
	writer, writerToken := w.person()
	authtest.GrantRole(t, w.ak, iam.GroupByID(studio.AuthScope), iam.UserSubject(writer.ID), w.writer)
	w.call(200, writerToken, "GET", "/me/entities", nil, &mine)
	if g := mine.Entities; len(g) != 1 || g[0].ID != studio.ID || !g[0].Writer || g[0].Editor || g[0].Owner || g[0].Member {
		t.Fatalf("the writer's groups %+v", mine)
	}
	w.call(200, strangerToken, "GET", "/me/entities", nil, &mine)
	if len(mine.Entities) != 0 {
		t.Fatalf("a stranger's groups %+v", mine)
	}
	w.call(403, strangerToken, "PATCH", path, entities.EntityUpdate{Names: name("Mine")}, nil)
	w.call(403, strangerToken, "DELETE", path, nil, nil)
	w.call(200, staffToken, "PATCH", path, entities.EntityUpdate{Slug: ptr("studio-two")}, nil)

	var deleted entities.Entity
	w.call(200, editorToken, "DELETE", path, nil, &deleted)
	if deleted.State != entities.StateDeleted || deleted.AuthScope != "" {
		t.Fatalf("deleted %+v", deleted)
	}
	if g, _ := w.scope(string(studio.ID)); g.DeletedAt == nil {
		t.Fatal("the deleted channel's scope is live")
	}
	w.call(403, editorToken, "PATCH", path, entities.EntityUpdate{State: ptr(entities.StateActive)}, nil)
	w.call(201, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "third"}}, nil)
	w.call(409, creatorToken, "PATCH", path, entities.EntityUpdate{State: ptr(entities.StateActive)}, nil)
	var restored entities.Entity
	w.call(200, staffToken, "PATCH", path, entities.EntityUpdate{State: ptr(entities.StateActive)}, &restored)
	if restored.AuthScope != string(studio.ID) {
		t.Fatalf("restored %+v", restored)
	}
	g, roles = w.scope(restored.AuthScope, creator, editor)
	if g.DeletedAt != nil || roles[iam.UserSubject(creator.ID)] != g.Persona.OwnerRole() || len(roles) != 1 {
		t.Fatalf("restored scope %+v roles %v: a new team with its owner", g, roles)
	}

	// The reconcile provisions what a write without Scopes left, and
	// repeating a sync changes nothing.
	bare, err := w.bare.CreateEntities(ctx, []entities.EntityInput{{Kind: "channel", Slug: "imported", OwnerUserID: creator.ID}})
	if err != nil || bare[0].AuthScope != "" {
		t.Fatalf("bare %+v %v", bare, err)
	}
	if err := w.store.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SyncScopes(ctx, []entities.EntityID{bare[0].ID, studio.ID}); err != nil {
		t.Fatal(err)
	}
	now, err := w.store.Entities(ctx, []entities.EntityID{bare[0].ID})
	if err != nil || now[0].AuthScope != string(bare[0].ID) {
		t.Fatalf("reconciled %+v %v", now, err)
	}
	if g, roles := w.scope(now[0].AuthScope, creator); g.DeletedAt != nil || len(roles) != 1 {
		t.Fatalf("reconciled scope %+v %v", g, roles)
	}
	if err := ckauthkit.Scopes(w.ak).Provision(ctx, []entities.Scope{{ID: now[0].AuthScope, Kind: "channel", Owner: editor.ID}}); err != nil {
		t.Fatalf("provisioning a live scope again: %v", err)
	}
	if err := ckauthkit.Scopes(w.ak).Retire(ctx, []string{now[0].AuthScope, now[0].AuthScope}); err != nil {
		t.Fatalf("retiring twice: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
