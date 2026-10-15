package content

import (
	"errors"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

func TestForeignOperatorCannotBecomeInteractionParticipant(t *testing.T) {
	operator := access.Actor{ID: "source-operator", Kind: auth.SubjectUser, IP: "192.0.2.1", Account: &access.AccountMembership{}}
	for _, anonymousAllowed := range []bool{false, true} {
		if err := participant(operator, anonymousAllowed); err == nil {
			t.Fatal("a nonmember operator became an audience participant")
		}
	}
	if user, ip, ok := reactionKey(operator); ok || user != "" || ip != "" {
		t.Fatalf("operator was silently made an anonymous reactor: %q %q %v", user, ip, ok)
	}
	if id := viewerID(operator); id != "" {
		t.Fatalf("operator obtained source-account ownership in target tenant: %q", id)
	}
	// Refuse before touching any content store, even through native methods.
	if _, err := (&comments{}).create(t.Context(), operator, contentref.ContentRef{}, CommentInput{}); !errors.Is(err, errForbidden) {
		t.Fatalf("native comment accepted external actor: %v", err)
	}
	if _, err := (&reactions{}).react(t.Context(), operator, contentref.ContentRef{}, 1); !errors.Is(err, errForbidden) {
		t.Fatalf("native reaction accepted external actor: %v", err)
	}
	if _, err := (&polls{}).vote(t.Context(), operator, "", ""); !errors.Is(err, errForbidden) {
		t.Fatalf("native poll vote accepted external actor: %v", err)
	}
}
