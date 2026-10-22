package kinds

import "testing"

func TestEntityMembershipScopes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		kind             GroupKind
		content, version bool
	}{
		{"default", GroupKind{}, true, false},
		{"legacy version", GroupKind{VersionLevel: true}, false, true},
		{"shared", GroupKind{MembershipScopes: []MembershipScope{ContentScope}}, true, false},
		{"version", GroupKind{MembershipScopes: []MembershipScope{VersionScope}}, false, true},
		{"both", GroupKind{MembershipScopes: []MembershipScope{ContentScope, VersionScope}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind.AllowsMembership(ContentScope) != tc.content || tc.kind.AllowsMembership(VersionScope) != tc.version {
				t.Fatal("incorrect allowed ownership scopes")
			}
		})
	}
	for _, scopes := range [][]MembershipScope{{"unknown"}, {ContentScope, ContentScope}} {
		cfg := doujins()
		kind := cfg.GroupKinds["tag"]
		kind.MembershipScopes = scopes
		cfg.GroupKinds["tag"] = kind
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid scopes accepted: %v", scopes)
		}
	}
}
