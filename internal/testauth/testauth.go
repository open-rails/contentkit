// Package testauth is an auth.Authenticator for ContentKit's package tests,
// which cannot import a real provider: a request's bearer names its actor
// (As) and an Authorizer answers its Can. The end-to-end suite (e2e/) runs
// the same gates against a real AuthKit.
package testauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
)

// Issuer is the test credentials' issuer.
const Issuer = "https://auth.test"

// Scope is the test Authenticator's root scope, where staff hold their
// permissions.
var Scope = auth.Scope{Authority: Issuer, ID: "root"}

// Authorizer decides a test actor's permissions in Scope.
type Authorizer interface {
	Can(ctx context.Context, a access.Actor, perm string) (bool, error)
}

// All holds every permission.
type All struct{}

func (All) Can(context.Context, access.Actor, string) (bool, error) { return true, nil }

// None holds nothing.
type None struct{}

func (None) Can(context.Context, access.Actor, string) (bool, error) { return false, nil }

// Auth authenticates "Bearer user:{id}" and "Bearer app:{id}"; any other
// bearer is refused. Authz answers Can in Scope; nil holds nothing.
type Auth struct{ Authz Authorizer }

// RootScope is Scope (authkit/auth Root).
func (Auth) RootScope(context.Context) (auth.Scope, error) { return Scope, nil }

func (a Auth) Authenticate(r *http.Request) (auth.Verified, error) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	kind, id, _ := strings.Cut(bearer, ":")
	sk := map[string]auth.SubjectKind{"user": auth.SubjectUser, "app": auth.SubjectApplication}[kind]
	if sk == "" || id == "" {
		return nil, auth.ErrUnauthenticated
	}
	return verified{id: auth.Identity{Issuer: Issuer, Subject: id, SubjectKind: sk,
		Invoker: auth.Invoker{Issuer: Issuer, ID: id}, Credential: auth.Credential{Kind: auth.CredentialSession}}, authz: a.Authz}, nil
}

type verified struct {
	id    auth.Identity
	authz Authorizer
}

func (v verified) Identity() auth.Identity { return v.id }

func (v verified) Can(ctx context.Context, scope auth.Scope, perm string) (bool, error) {
	if scope != Scope || v.authz == nil {
		return false, nil
	}
	return v.authz.Can(ctx, access.Actor{ID: v.id.Subject, Kind: v.id.SubjectKind}, perm)
}

// Token is the bearer As sends for a signed-in actor.
func Token(a access.Actor) string {
	if a.Kind == auth.SubjectApplication {
		return "app:" + a.ID
	}
	return "user:" + a.ID
}

// As is r sent by a: a signed-in actor's bearer, and a.IP as the peer
// address.
func As(r *http.Request, a access.Actor) *http.Request {
	if !a.Anonymous && a.ID != "" {
		r.Header.Set("Authorization", "Bearer "+Token(a))
	}
	if a.IP != "" {
		r.RemoteAddr = a.IP + ":1234"
	}
	return r
}

// Verify is what an Authenticator says of a request sent by a: anonymous is
// auth.ErrUnauthenticated, any other a Verified answering Can by authz.
func Verify(a access.Actor, authz Authorizer) (auth.Verified, error) {
	if a.Anonymous || a.ID == "" {
		return nil, auth.ErrUnauthenticated
	}
	return Actor(a, authz).Verified, nil
}

// Actor is a as the gate builds it, with a Verified answering Can by authz,
// for calls that take an actor directly.
func Actor(a access.Actor, authz Authorizer) access.Actor {
	if a.Anonymous || a.ID == "" {
		return a
	}
	if a.Kind == "" {
		a.Kind = auth.SubjectUser
	}
	a.Verified = verified{id: auth.Identity{Issuer: Issuer, Subject: a.ID, SubjectKind: a.Kind,
		Invoker: auth.Invoker{Issuer: Issuer, ID: a.ID}, Credential: auth.Credential{Kind: auth.CredentialSession}}, authz: authz}
	return a
}
