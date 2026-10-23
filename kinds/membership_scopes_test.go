package kinds

import "testing"

func TestEntityMembershipScopes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             EntityKind
		content, version bool
	}{
		{"default", EntityKind{}, true, false},
		{"legacy version", EntityKind{VersionLevel: true}, false, true},
		{"shared", EntityKind{MembershipScopes: []MembershipScope{ContentScope}}, true, false},
		{"version", EntityKind{MembershipScopes: []MembershipScope{VersionScope}}, false, true},
		{"both", EntityKind{MembershipScopes: []MembershipScope{ContentScope, VersionScope}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind.AllowsMembership(ContentScope) != tc.content || tc.kind.AllowsMembership(VersionScope) != tc.version {
				t.Fatal("incorrect allowed ownership scopes")
			}
		})
	}
	for _, scopes := range [][]MembershipScope{{"unknown"}, {ContentScope, ContentScope}} {
		cfg := doujins()
		kind := cfg.EntityKinds["tag"]
		kind.MembershipScopes = scopes
		cfg.EntityKinds["tag"] = kind
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid scopes accepted: %v", scopes)
		}
	}
}
