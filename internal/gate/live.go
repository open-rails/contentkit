package gate

import (
	"context"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
)

// LiveActor rechecks a stream's credential and app membership without spending
// its sender proof twice. PermissionChecker checks credential expiry/revocation
// even when the queried permission is not granted. Each observation gets a new
// permission memo; owner shortcuts must not bypass this credential check.
func LiveActor(ctx context.Context, provider auth.Authenticator, actor access.Actor) (access.Actor, error) {
	if IsHost(actor) {
		return actor, nil
	}
	if actor.Anonymous || actor.Verified == nil {
		return access.Actor{}, auth.ErrUnauthenticated
	}
	verified := actor.Verified
	if m, ok := verified.(*memo); ok {
		verified = m.v
	}
	if _, ok := verified.(auth.PermissionChecker); !ok {
		return access.Actor{}, auth.ErrUnavailable
	}
	actor.Verified = &memo{v: verified}
	if _, err := Can(ctx, provider, actor, access.ContentWrite.String()); err != nil {
		return access.Actor{}, err
	}
	if scoped, ok := provider.(ScopedAuthorization); ok {
		id, err := resolveActor(ctx, scoped, verified)
		if err != nil {
			return access.Actor{}, err
		}
		if id != actor.ID {
			return access.Actor{}, auth.ErrForbidden
		}
		if accounts, ok := provider.(ScopedAccounts); ok {
			id, member, err := resolveTenantAccount(ctx, accounts, verified, actor.ID)
			if err != nil {
				return access.Actor{}, err
			}
			actor.Account = &access.AccountMembership{}
			if member {
				actor.Account.ID = id
			}
		}
	}
	return actor, nil
}
