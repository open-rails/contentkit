package gate

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
)

type streamAuth struct{ credential *streamCredential }

func (a streamAuth) Authenticate(*http.Request) (auth.Verified, error) {
	panic("sender proof must not be spent twice")
}
func (a streamAuth) RootScope(context.Context) (auth.Scope, error) {
	return auth.Scope{Authority: "issuer", ID: "root"}, nil
}

type streamCredential struct {
	failure error
	checks  int
}

func (v *streamCredential) Identity() auth.Identity {
	return auth.Identity{Subject: "owner", SubjectKind: auth.SubjectUser}
}
func (v *streamCredential) Can(context.Context, auth.Scope, string) (bool, error) {
	v.checks++
	return false, v.failure
}
func TestLiveActorChecksRevocationWithoutAuthenticatingAgain(t *testing.T) {
	v := &streamCredential{}
	a := streamAuth{v}
	actor := access.Actor{ID: "owner", Kind: auth.SubjectUser, Verified: &memo{v: v}}
	for _, failure := range []error{nil, errors.Join(auth.ErrUnauthenticated, auth.ErrExpired), errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)} {
		v.failure = failure
		_, err := LiveActor(t.Context(), a, actor)
		if !errors.Is(err, failure) {
			t.Fatalf("got %v want %v", err, failure)
		}
	}
	if v.checks != 3 {
		t.Fatalf("credential check memoized: %d", v.checks)
	}
}
