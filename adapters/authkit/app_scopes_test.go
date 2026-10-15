package authkit_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/groups"
)

func TestAppGroupScopesIsolateSameGroupID(t *testing.T) {
	ctx := t.Context()
	r := authkit.NewRoles()
	channel := r.Persona("channel")
	permissions := channel.Declare(access.GroupPermissions("channel")...)
	editorRole := channel.Role("editor", permissions[1])
	r.Root.Role("global-editor", permissions[1])
	tenants := []contentkit.TenantID{contentkit.NewTenantID(), contentkit.NewTenantID()}
	firstTenant, _ := iam.ParseTenantID(tenants[0].String())
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles, c.DefaultTenantID = r, firstTenant }))
	owner, editor := authtest.NewUser(t, ak), authtest.NewUser(t, ak)
	bare := uuid.NewString()
	backends := []groups.Scopes{}
	providerIDs := map[string]bool{}
	providerClients := map[string]*authkit.Client{}
	var firstID string
	for _, tenant := range tenants {
		tenantID, _ := iam.ParseTenantID(tenant.String())
		if err := ak.CreateTenant(ctx, iam.Tenant{ID: tenantID}); err != nil {
			t.Fatal(err)
		}
		client, err := ak.ForTenant(ctx, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		localOwner := owner
		if tenant != tenants[0] {
			localOwner = authtest.NewUser(t, client)
		}
		for range 2 {
			scope := contentkit.Scope{TenantID: tenant, AppID: contentkit.NewAppID()}
			backend, err := ckauthkit.AppScopes(client, scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Provision(ctx, []groups.Scope{{ID: bare, Kind: "channel", Owner: localOwner.ID}}); err != nil {
				t.Fatal(err)
			}
			id, err := ckauthkit.GroupScopeID(scope, bare)
			if err != nil || providerIDs[id] {
				t.Fatalf("scope ID collision: %s %v", id, err)
			}
			providerIDs[id] = true
			providerClients[id] = client
			if firstID == "" {
				firstID = id
			}
			backends = append(backends, backend)
		}
	}
	if _, err := ak.Group(ctx, iam.GroupByID(bare)); !errors.Is(err, iam.ErrGroupNotFound) {
		t.Fatalf("bare provider scope created: %v", err)
	}
	authtest.GrantRole(t, ak, iam.GroupByID(firstID), iam.UserSubject(editor.ID), editorRole)
	check := func(index int, want bool) {
		t.Helper()
		got, err := backends[index].(groups.LocalScopes).HoldingScopes(ctx, editor.ID, []string{"channel"}, []string{bare})
		if err != nil || (len(got[bare]) > 0) != want {
			t.Fatalf("scope %d holdings %+v err %v", index, got, err)
		}
	}
	for i := range backends {
		check(i, i == 0)
	}
	if err := ak.RemoveGroupMember(ctx, iam.SystemIdentity(), iam.GroupByID(firstID), iam.UserSubject(editor.ID)); err != nil {
		t.Fatal(err)
	}
	check(0, false)
	authtest.GrantRole(t, ak, iam.GroupByID(firstID), iam.UserSubject(editor.ID), editorRole)
	check(0, true)
	if err := backends[0].Retire(ctx, []string{bare}); err != nil {
		t.Fatal(err)
	}
	for i := range backends {
		check(i, false)
	}
	for id := range providerIDs {
		g, err := providerClients[id].Group(ctx, iam.GroupByID(id))
		if err != nil || (g.DeletedAt != nil) != (id == firstID) {
			t.Fatalf("retirement crossed scopes: %+v %v", g, err)
		}
	}
	if err := backends[0].Provision(ctx, []groups.Scope{{ID: bare, Kind: "channel", Owner: owner.ID}}); err != nil {
		t.Fatal(err)
	}
	check(0, false) // restoring a group does not restore the retired team's grants.
	authtest.GrantRole(t, ak, iam.RootGroup(), iam.UserSubject(editor.ID), mustRole(t, ak, "root:global-editor"))
	for i := range backends {
		check(i, true) // deliberately global grants inherit in every live group.
	}
}
