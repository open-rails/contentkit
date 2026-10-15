package contentkit

import (
	"github.com/open-rails/contentkit/notification"
	"testing"
)

func TestAccountSpaceSharesIdentityWithoutMutatingSiteConfiguration(t *testing.T) {
	source := Config{Tenant: "site-a", Database: DatabaseConfig{Schema: "site_a"}, Accounts: &AccountsConfig{Namespace: "shared_accounts", Schema: "account_notifications", Avatars: &AvatarsConfig{Primary: "https://primary.test/api/v1/contentkit", Key: "service-key"}}}
	got, err := source.withAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if got.Tenant != source.Tenant || got.Database.Schema != source.Database.Schema {
		t.Fatal("account space changed the catalog")
	}
	if got.Notifications.Namespace != "shared_accounts" || got.Notifications.Schema != "account_notifications" {
		t.Fatalf("inbox space %+v", got.Notifications)
	}
	kind, _ := got.Avatars.kind()
	if kind.Namespace != "shared_accounts" || got.Avatars.Primary != source.Accounts.Avatars.Primary {
		t.Fatalf("avatar space %+v", got.Avatars)
	}
	if source.Notifications != nil || source.Avatars != nil || source.Accounts.Avatars.Kind != nil {
		t.Fatal("caller configuration mutated")
	}
	if _, err := got.withAccounts(); err != nil {
		t.Fatalf("resolved configuration cannot be reused: %v", err)
	}
}

func TestAccountSpaceRejectsConflictingOwnership(t *testing.T) {
	for _, c := range []Config{
		{Tenant: "accounts", Accounts: &AccountsConfig{Namespace: "accounts", Schema: "shared"}},
		{Tenant: "site", Accounts: &AccountsConfig{Namespace: "accounts", Schema: "bad.schema"}},
		{Tenant: "site", Accounts: &AccountsConfig{Namespace: "accounts", Schema: "shared"}, Notifications: &notification.Config{Namespace: "other", Schema: "other"}},
		{Tenant: "site", Accounts: &AccountsConfig{Namespace: "accounts", Schema: "shared", Avatars: &AvatarsConfig{}}, Avatars: &AvatarsConfig{}},
	} {
		if _, err := c.withAccounts(); err == nil {
			t.Fatalf("accepted conflicting config %+v", c)
		}
	}
}
