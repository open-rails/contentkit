package versions

import (
	"context"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

func TestArchiveClaimProofPreservesAudienceBounds(t *testing.T) {
	gate, err := access.NewGate(access.GateConfig{Tenant: "test", Kinds: kinds.Config{
		Tiers: []string{"premium"}, TokenClaims: []string{"premium"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := contentref.NewID()
	proof := archiveClaimProof{identity: auth.Identity{Subject: id, SubjectKind: auth.SubjectApplication}, claims: []string{"premium"}}
	if _, ok := any(proof).(auth.PermissionChecker); ok {
		t.Fatal("archive entitlement proof acquired general permission authority")
	}
	for _, tc := range []struct {
		name    string
		account *access.AccountMembership
		allowed bool
	}{
		{"legacy application", nil, true},
		{"bound application", &access.AccountMembership{ID: contentref.NewID()}, true},
		{"foreign nonmember", &access.AccountMembership{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := access.Actor{ID: id, Kind: auth.SubjectApplication, Account: tc.account, Verified: proof}
			decisions, err := gate.Decide(context.Background(), actor, []access.Item{{Content: contentref.NewID(),
				Rule: access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}}})
			if err != nil || len(decisions) != 1 || decisions[0].Allowed != tc.allowed {
				t.Fatalf("audience entitlement: %+v %v, want allowed=%v", decisions, err, tc.allowed)
			}
		})
	}
	request := ArchiveRequest{Target: Target{Content: contentref.NewID(), Version: contentref.NewID()}, Viewer: id,
		Kind: auth.SubjectApplication, Claims: []string{"premium"}, Revision: "revision", Name: "book.zip"}
	legacy := archiveScope("test", request)
	request.AccountSet = true
	if archiveScope("test", request) == legacy {
		t.Fatal("signed scope erased explicit nonmembership into legacy identity")
	}
	request.Account = contentref.NewID()
	if archiveScope("test", request) == legacy {
		t.Fatal("signed scope omitted a bound application account")
	}
}
