// Package access is ContentKit's access model: the Actor; the key grammar
// (<tenant>:content:<id>, <tenant>:content:<id>/<version>, <tenant>:group:<id>
// and configured tiers); rules stored on content, versions and groups; the
// Gate deciding versions from one billing read per request, its listing
// Filter and SQL over content_facets; and the Billing dependency.
package access

import (
	"context"

	"github.com/open-rails/authkit/auth"
)

// Actor is the caller. ContentKit's routes build it from the host's
// auth.Authenticator: the Verified's Identity, or anonymous when the request
// carries no credential.
type Actor struct {
	ID string // the subject (auth.Identity.Subject); empty when Anonymous
	// Account records membership in the engine's tenant independently of the
	// source actor. A nonnil empty membership is a verified actor from another
	// tenant; it grants no audience identity or ownership shortcut here.
	// Nil retains the legacy host contract that a person's ID is its account ID.
	Account *AccountMembership
	// Kind is the subject's kind: a person (auth.SubjectUser) or an
	// application (auth.SubjectApplication).
	Kind auth.SubjectKind
	// Invoker is who acts on the subject's behalf; zero when the subject acts
	// for itself.
	Invoker auth.Invoker
	// IP is the client's address (the request's RemoteAddr): the key of an
	// anonymous actor's reactions, votes and limits.
	IP        string
	Anonymous bool
	// Verified is the request's verified credential, kept for that request
	// alone: Can asks it. Nil for an anonymous actor.
	Verified auth.Verified
}

// AccountMembership identifies an existing account in the target tenant.
// ID is empty for an actor who can administer the tenant but is not a member.
type AccountMembership struct{ ID string }

// Person reports a signed-in person: a subject that is not an application.
func (a Actor) Person() bool {
	return !a.Anonymous && a.ID != "" && a.Kind != auth.SubjectApplication
}

// AccountID is the actor's audience account in the current tenant, if any.
// A global operator keeps its source ID for audit but has no implicit account
// in a different tenant.
func (a Actor) AccountID() string {
	if !a.Person() {
		return ""
	}
	if a.Account != nil {
		return a.Account.ID
	}
	return a.ID
}

// Member reports a person with an account in the engine's tenant.
func (a Actor) Member() bool { return a.AccountID() != "" }

// Can reports whether the actor holds exactly permission in scope, asking its
// Verified (auth.PermissionChecker). Anonymous, without a Verified or without
// the capability it holds nothing. An error never grants.
func (a Actor) Can(ctx context.Context, scope auth.Scope, permission string) (bool, error) {
	if a.Anonymous || a.Verified == nil || permission == "" {
		return false, nil
	}
	pc, ok := a.Verified.(auth.PermissionChecker)
	if !ok {
		return false, nil
	}
	return pc.Can(ctx, scope, permission)
}

// Entitled is an optional capability of an actor's auth.Verified: the
// entitlement claims its verified token carries. The Gate answers a tier in
// kinds.Config.TokenClaims from them without a billing read, so a refund
// keeps such a tier until the token expires.
type Entitled interface {
	Entitlements() []string
}

// claims is the actor's token entitlement claims.
func (a Actor) claims() []string {
	if e, ok := a.Verified.(Entitled); ok && !a.Anonymous {
		return e.Entitlements()
	}
	return nil
}
