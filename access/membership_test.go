package access

import (
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/kinds"
)

func TestActorSeparatesSourceIdentityFromTenantMembership(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actor   Actor
		person  bool
		account string
	}{
		{"legacy member", Actor{ID: "person", Kind: auth.SubjectUser}, true, "person"},
		{"tenant member", Actor{ID: "source", Kind: auth.SubjectUser, Account: &AccountMembership{ID: "account"}}, true, "account"},
		{"external operator", Actor{ID: "source", Kind: auth.SubjectUser, Account: &AccountMembership{}}, true, ""},
		{"service", Actor{ID: "service", Kind: auth.SubjectApplication}, false, ""},
		{"anonymous", Actor{Anonymous: true}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.actor.Person() != tc.person || tc.actor.AccountID() != tc.account || tc.actor.Member() != (tc.account != "") {
				t.Fatalf("person=%v account=%q member=%v", tc.actor.Person(), tc.actor.AccountID(), tc.actor.Member())
			}
		})
	}
}

type membershipClaims struct{}

func (membershipClaims) Identity() auth.Identity {
	return auth.Identity{Subject: "source", SubjectKind: auth.SubjectUser}
}
func (membershipClaims) Entitlements() []string { return []string{"premium"} }

func TestForeignActorCannotReuseSourceTenantEntitlements(t *testing.T) {
	g, err := NewGate(GateConfig{Tenant: "target", Kinds: kinds.Config{Tiers: []string{"premium"}, TokenClaims: []string{"premium"}}})
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "source", Kind: auth.SubjectUser, Verified: membershipClaims{}, Account: &AccountMembership{}}
	items := []Item{{Content: "01920000-0000-7000-8000-000000000001", Rule: Rule{Level: TierLevel, Tiers: []string{"premium"}}}}
	for _, member := range []bool{false, true} {
		if member {
			actor.Account.ID = "source"
		}
		decisions, err := g.Decide(t.Context(), actor, items)
		if err != nil || len(decisions) != 1 || decisions[0].Allowed != member {
			t.Fatalf("member=%v decisions=%+v err=%v", member, decisions, err)
		}
		filter, err := g.Filter(t.Context(), actor)
		if err != nil || filter.Tier("premium") != member {
			t.Fatalf("member=%v filter=%+v err=%v", member, filter, err)
		}
	}
}
