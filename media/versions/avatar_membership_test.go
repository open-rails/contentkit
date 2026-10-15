package versions

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestAvatarEditorRequiresTargetTenantAccount(t *testing.T) {
	authn := testauth.Auth{Authz: testauth.All{}}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer user:operator-source")
	verified, err := authn.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	operator := access.Actor{ID: "operator-source", Kind: verified.Identity().SubjectKind, Verified: verified, Account: &access.AccountMembership{}}
	unavailable := errors.New("directory unavailable")
	s := Service{o: Options{Auth: authn, AccountExists: func(_ context.Context, id string) (bool, error) {
		if id == "unavailable" {
			return false, unavailable
		}
		return id == "tenant-member", nil
	}}}
	if allowed, err := s.avatarEditor(t.Context(), operator, "operator-source"); allowed || err != nil {
		t.Fatalf("foreign source account became a target avatar: %v %v", allowed, err)
	}
	if allowed, err := s.avatarEditor(t.Context(), operator, "tenant-member"); !allowed || err != nil {
		t.Fatalf("root could not edit an existing tenant account: %v %v", allowed, err)
	}
	if allowed, err := s.avatarEditor(t.Context(), operator, "unavailable"); allowed || !errors.Is(err, unavailable) {
		t.Fatalf("directory outage changed authorization: %v %v", allowed, err)
	}
}
