package critical_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
)

// This decorator measures calls while every authority answer comes from the
// real AuthKit/PostgreSQL runtime. It supplies no canned grants or group rows.
type countedChannelDirectory struct {
	*authkit.Client
	effective, groups, checks int
}

func (c *countedChannelDirectory) Group(ctx context.Context, ref iam.GroupRef) (iam.Group, error) {
	c.groups++
	return c.Client.Group(ctx, ref)
}
func (c *countedChannelDirectory) Can(ctx context.Context, who auth.Identity, ref iam.GroupRef, perm iam.Perm) (bool, error) {
	c.checks++
	return c.Client.Can(ctx, who, ref, perm)
}
func (c *countedChannelDirectory) EffectivePermissions(ctx context.Context, who auth.Identity, refs []iam.GroupRef) (map[string][]iam.Perm, error) {
	c.effective++
	return c.Client.EffectivePermissions(ctx, who, refs)
}

func TestChannelAuthorityUsesBatchedCurrentGrants(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	account, _ := w.person(t, 0)
	account.TOTP = authtest.EnrollTOTP(t, w.tenant[0], account)
	persona, err := w.auth.Persona("channel")
	must(t, err)
	tenant, err := iam.ParseTenantID(a.cfg.Scope.TenantID.String())
	must(t, err)
	directory := &countedChannelDirectory{Client: w.auth}
	provider, err := ckauth.AppScopes(directory, *a.cfg.Scope)
	must(t, err)
	local := provider.(entities.LocalScopes)
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = contentref.NewID()
	}
	const existing = 32
	for _, id := range ids[:existing] {
		pid, err := ckauth.ChannelScopeID(*a.cfg.Scope, id)
		must(t, err)
		_, err = w.auth.CreateGroup(t.Context(), iam.NewGroup{ID: pid, TenantID: tenant, Persona: persona})
		must(t, err)
	}
	foreignID := ids[existing]
	foreignProvider, err := ckauth.ChannelScopeID(*a.cfg.Scope, foreignID)
	must(t, err)
	foreignTenant, err := iam.ParseTenantID(w.apps[2].cfg.Scope.TenantID.String())
	must(t, err)
	_, err = w.auth.CreateGroup(t.Context(), iam.NewGroup{ID: foreignProvider, TenantID: foreignTenant, Persona: persona})
	must(t, err)
	lookup := func() map[string][]string {
		t.Helper()
		directory.effective, directory.groups, directory.checks = 0, 0, 0
		started := time.Now()
		got, err := local.HoldingScopes(t.Context(), account.ID, []string{"channel"}, ids)
		t.Logf("authority lookup: candidates=%d results=%d batches=%d groups=%d Can=%d elapsed=%s", len(ids), len(got), directory.effective, directory.groups, directory.checks, time.Since(started))
		must(t, err)
		if directory.effective != 2 || directory.checks != 0 {
			t.Fatalf("unbounded authority query shape: batch=%d Can=%d", directory.effective, directory.checks)
		}
		return got
	}
	if got := lookup(); len(got) != 0 || directory.groups != 0 {
		t.Fatalf("unrelated channel grants or per-channel metadata work: %v groups=%d", got, directory.groups)
	}
	selectedProvider, err := ckauth.ChannelScopeID(*a.cfg.Scope, ids[0])
	must(t, err)
	selected := iam.GroupByID(selectedProvider)
	authtest.GrantRole(t, w.tenant[0], selected, iam.UserSubject(account.ID), w.channelContributor)
	got := lookup()
	if len(got) != 1 || directory.groups != 1 || !slices.Contains(got[ids[0]], access.ChannelContentCreate) || !slices.Contains(got[ids[0]], access.ChannelContentEditOwn) || slices.Contains(got[ids[0]], access.ChannelContentWrite) {
		t.Fatalf("narrow channel role did not retain exact live capabilities: %+v metadata=%d", got, directory.groups)
	}
	authtest.RevokeRole(t, w.tenant[0], selected, iam.UserSubject(account.ID), w.channelContributor)
	if got = lookup(); len(got) != 0 {
		t.Fatalf("revoked grant retained: %v", got)
	}
	// Inherited global authority is not the same as direct channel membership.
	authtest.GrantRole(t, w.auth, iam.RootGroup(), iam.UserSubject(account.ID), iam.RootPersona().OwnerRole())
	got = lookup()
	if len(got) != existing || len(got[foreignID]) != 0 {
		t.Fatalf("inherited authority lost local scopes or included foreign tenant: got%d want%d foreign=%v", len(got), existing, got[foreignID])
	}
	for i, id := range ids[:existing] {
		for _, permission := range access.ChannelPermissions() {
			if !slices.Contains(got[id], permission) {
				t.Fatal(fmt.Sprintf("scope%d missing inherited capability %s: %v", i, permission, got[id]))
			}
		}
	}
}
