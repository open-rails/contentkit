package critical_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// This wrapper controls only a provider acknowledgement, not authorization:
// every group and permission is still created/read through real AuthKit.
type configuredScopes struct {
	entities.ScopeProvisioner
	scope         ck.Scope
	before, after func(context.Context) error
}

func (s configuredScopes) ContentKitScope() (string, string) {
	return s.scope.TenantID.String(), s.scope.AppID.String()
}

func (s configuredScopes) Provision(ctx context.Context, groups []entities.Scope) error {
	if s.before != nil {
		if err := s.before(ctx); err != nil {
			return err
		}
	}
	if err := s.ScopeProvisioner.Provision(ctx, groups); err != nil {
		return err
	}
	if s.after != nil {
		return s.after(ctx)
	}
	return nil
}

func TestConfiguredOwnerProvisioning(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Content.EntityKinds["channel"] = kinds.EntityKind{Route: "channels", Owner: "site", AuthScope: true}
	})
	a := w.apps[0]
	admin := a.engine.Client()
	// An ordinary creator-selected model has no fixed owner to invent.
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	page, err := admin.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
	must(t, err)
	if len(page.Entities) != 0 {
		t.Fatal("migration invented an undeclared channel")
	}
	post, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Existing", Body: "Preserve this body"}}})
	must(t, err)
	cfg := a.cfg
	cfg.Content.Kinds = maps.Clone(cfg.Content.Kinds)
	for _, name := range []string{"article", "gallery"} {
		kind := cfg.Content.Kinds[name]
		kind.OwnerEntity = &kinds.EntityRef{Kind: "channel", Slug: "blog"}
		cfg.Content.Kinds[name] = kind
	}
	for _, opt := range []ck.MigrationOptions{{}, {Scopes: w.apps[1].deps.Scopes}} {
		if err := ck.MigratePostgres(t.Context(), w.pool, cfg, opt); err == nil {
			t.Fatal("missing/foreign provider was accepted")
		}
	}
	page, err = admin.GetManageEntities(t.Context(), ck.GetManageEntitiesQuery{Kind: "channel"})
	must(t, err)
	if len(page.Entities) != 0 {
		t.Fatal("provider preflight wrote an entity")
	}

	// Taking this same pool's sole connection from inside Provision proves the
	// migration holds neither a transaction nor a borrowed connection there.
	pc := w.pool.Config()
	pc.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	must(t, err)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	lost := errors.New("provider reply lost after committing the group")
	provider := configuredScopes{ScopeProvisioner: a.deps.Scopes, scope: *cfg.Scope,
		before: pool.Ping, after: func(context.Context) error { return lost }}
	if err := ck.MigratePostgres(ctx, pool, cfg, ck.MigrationOptions{Scopes: provider}); !errors.Is(err, lost) {
		t.Fatalf("expected retryable provider failure: %v", err)
	}
	var id, pendingScope string
	must(t, pool.QueryRow(ctx, `SELECT id,coalesce(auth_scope,'') FROM `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND kind='channel' AND slug='blog'`, cfg.Scope.AppID.String()).Scan(&id, &pendingScope))
	if pendingScope != "" {
		t.Fatal("failed provisioning exposed authoring scope")
	}
	retained, err := admin.GetContent(ctx, post.ID, ck.GetContentQuery{})
	must(t, err)
	if retained.OwnerEntityID != "" || retained.Body != post.Body {
		t.Fatal("failed provisioning attached or changed existing content")
	}
	groupID, err := ckauth.EntityScopeID(*cfg.Scope, id)
	must(t, err)
	group, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	provider.after = nil
	must(t, ck.MigratePostgres(ctx, pool, cfg, ck.MigrationOptions{Scopes: provider}))
	page, err = admin.GetManageEntities(ctx, ck.GetManageEntitiesQuery{Kind: "channel"})
	must(t, err)
	if len(page.Entities) != 1 || string(page.Entities[0].ID) != id {
		t.Fatal("retry changed the reserved identity or duplicated a shared owner")
	}
	retained, err = admin.GetContent(ctx, post.ID, ck.GetContentQuery{})
	must(t, err)
	if retained.OwnerEntityID != id || retained.Body != post.Body || !reflect.DeepEqual(retained.Access, post.Access) {
		t.Fatal("migration lost content identity, body or access")
	}
	member, _ := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], iam.GroupByID(groupID), iam.UserSubject(member.ID), w.channelWriter)
	label := []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Staff-selected name"}}
	_, err = admin.PatchEntity(ctx, id, entities.EntityUpdate{Names: label})
	must(t, err)
	before, err := admin.GetManageEntity(ctx, id, ck.GetManageEntityQuery{})
	must(t, err)
	memberships, err := w.auth.ListMemberships(ctx, iam.UserSubject(member.ID), iam.PageRequest{})
	must(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ck.MigratePostgres(ctx, pool, cfg, ck.MigrationOptions{Scopes: provider})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	after, err := admin.GetManageEntity(ctx, id, ck.GetManageEntityQuery{})
	must(t, err)
	newGroup, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	newMemberships, err := w.auth.ListMemberships(ctx, iam.UserSubject(member.ID), iam.PageRequest{})
	must(t, err)
	if after.Revision != before.Revision || !reflect.DeepEqual(after.Names, before.Names) || newGroup.ID != group.ID || !reflect.DeepEqual(newMemberships, memberships) {
		t.Fatal("repeated/concurrent provisioning changed names, identities or editor assignments")
	}
	// A manually retired AuthKit group is not an invitation to recreate it and
	// wipe its grants on the next ordinary application migration.
	must(t, w.auth.DeleteGroup(ctx, iam.GroupByID(groupID)))
	deleted, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	if deleted.DeletedAt == nil {
		t.Fatal("fixture group was not retired")
	}
	if err := ck.MigratePostgres(ctx, pool, cfg, ck.MigrationOptions{Scopes: provider}); err == nil {
		t.Fatal("configuration silently revived a retired group")
	}
	stillDeleted, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	if stillDeleted.DeletedAt == nil || !reflect.DeepEqual(stillDeleted, deleted) {
		t.Fatal("refused provisioning altered the retired group")
	}
}

func TestConfiguredOwnerRechecksAfterProvisioning(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Content.EntityKinds["channel"] = kinds.EntityKind{Route: "channels", Owner: "site", AuthScope: true}
		kind := c.Content.Kinds["article"]
		kind.OwnerEntity = &kinds.EntityRef{Kind: "channel", Slug: "blog"}
		c.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	provider := configuredScopes{ScopeProvisioner: a.deps.Scopes, scope: *a.cfg.Scope, after: func(ctx context.Context) error {
		_, err := w.pool.Exec(ctx, `UPDATE `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` SET state='deleted',deleted_at=clock_timestamp() WHERE tenant_id=$1 AND kind='channel' AND slug='blog'`, a.cfg.Scope.AppID.String())
		return err
	}}
	if err := ck.MigratePostgres(t.Context(), w.pool, a.cfg, ck.MigrationOptions{Scopes: provider}); err == nil {
		t.Fatal("migration attached a channel deleted during provisioning")
	}
	var state, scope string
	must(t, w.pool.QueryRow(t.Context(), `SELECT state,coalesce(auth_scope,'') FROM `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND kind='channel' AND slug='blog'`, a.cfg.Scope.AppID.String()).Scan(&state, &scope))
	if state != "deleted" || scope != "" {
		t.Fatal("finalization revived or authorized a retired entity")
	}
}

func TestConfiguredOwnerKeylessAdapter(t *testing.T) {
	w := newWorld(t, func(c *ck.Config) {
		c.Content.EntityKinds["channel"] = kinds.EntityKind{Route: "channels", Owner: "site", AuthScope: true}
		kind := c.Content.Kinds["article"]
		kind.OwnerEntity = &kinds.EntityRef{Kind: "channel", Slug: "blog"}
		c.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	var authConfig authkit.Config
	// Capture the real fixture's public store configuration; the provisioning
	// adapter receives only store/role data, no keys or runtime dependencies.
	_ = authtest.Replica(t, w.auth, authtest.WithConfig(func(c *authkit.Config) {
		authConfig = authkit.Config{Database: c.Database, Roles: c.Roles}
	}))
	pc := w.pool.Config()
	pc.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	must(t, err)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	provider, err := ckauth.MigrationScopes(pool, authConfig, *a.cfg.Scope)
	must(t, err)
	must(t, ck.MigratePostgres(ctx, pool, a.cfg, ck.MigrationOptions{Scopes: provider}))
	channels, err := a.engine.Client().GetManageEntities(ctx, ck.GetManageEntitiesQuery{Kind: "channel"})
	must(t, err)
	if len(channels.Entities) != 1 {
		t.Fatal("keyless migration did not create its channel")
	}
	id := string(channels.Entities[0].ID)
	groupID, err := ckauth.EntityScopeID(*a.cfg.Scope, id)
	must(t, err)
	group, err := w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	if group.TenantID.String() != a.cfg.Scope.TenantID.String() || group.Persona.String() != "channel" {
		t.Fatal("keyless provisioning crossed the app identity")
	}
	member, token := w.person(t, 0)
	writer := a.client(token)
	input := catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Declared model", Body: "No host seed step"}}}
	_, err = writer.PostContents(ctx, input)
	requireError(t, err, 403, "forbidden")
	authtest.GrantRole(t, w.tenant[0], iam.GroupByID(groupID), iam.UserSubject(member.ID), w.channelWriter)
	must(t, ck.MigratePostgres(ctx, pool, a.cfg, ck.MigrationOptions{Scopes: provider}))
	post, err := writer.PostContents(ctx, input)
	must(t, err)
	if post.OwnerEntityID != id {
		t.Fatal("declared channel did not own its post")
	}
	must(t, w.auth.DeleteGroup(ctx, iam.GroupByID(groupID)))
	if err := ck.MigratePostgres(ctx, pool, a.cfg, ck.MigrationOptions{Scopes: provider}); err == nil {
		t.Fatal("keyless migration revived a retired group")
	}
	group, err = w.auth.Group(ctx, iam.GroupByID(groupID))
	must(t, err)
	if group.DeletedAt == nil {
		t.Fatal("keyless migration removed the group tombstone")
	}
}
