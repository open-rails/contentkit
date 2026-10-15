package discovery

import (
	"errors"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
)

func TestForeignOperatorCannotRecordAnonymousHistory(t *testing.T) {
	s := Service{o: Options{Tenant: "target"}}
	actor := access.Actor{ID: "source-operator", Kind: auth.SubjectUser, Account: &access.AccountMembership{}}
	if _, err := s.subject(actor, "browser-session"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("operator was recorded as a target-tenant visitor: %v", err)
	}
	if person(actor) != "" {
		t.Fatal("source identity leaked into target personalization")
	}
	for _, session := range []string{"", "browser-session"} {
		if err := s.RecordExposures(t.Context(), actor, Exposures{Session: session}); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("operator exposure with session %q was not refused: %v", session, err)
		}
	}
}
