package authkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
)

// publishingPermissions is implemented by AuthKit's native Client. These are
// trusted host operations; the publishing request never supplies an Identity.
type publishingPermissions interface {
	Permission(string) (iam.Perm, error)
	Can(context.Context, auth.Identity, iam.GroupRef, iam.Perm) (bool, error)
}

type publishingCredential struct {
	Version     int      `json:"version"`
	Tenant      string   `json:"tenant"`
	App         string   `json:"app"`
	Issuer      string   `json:"issuer"`
	Key         string   `json:"key"`
	Actor       string   `json:"actor"`
	Permissions []string `json:"permissions"`
}

var _ contentkit.PublishingAuthorization = authorization{}

// CapturePublishing supports native, app-bound API keys. It retains only the
// key ID and the permissions the original request actually held. Interactive
// credentials and foreign providers need their own durable delegation policy.
func (a authorization) CapturePublishing(ctx context.Context, scope contentkit.Scope, v auth.Verified) (json.RawMessage, error) {
	if err := a.admit(scope, v); err != nil {
		return nil, err
	}
	// A foreign identity issuer needs its own replay policy. A coincident key
	// ID there must never become authority in this local credential database.
	if a.cfg.Issuer != a.cfg.PermissionAuthority {
		return nil, auth.ErrForbidden
	}
	id := v.Identity()
	state, ok := iam.StateOf(id)
	bound, hasBound := v.(auth.Bound)
	checker, hasChecker := v.(auth.PermissionChecker)
	native, hasNative := a.groups.(publishingPermissions)
	if !ok || !state.IsAPIKey() || id.Credential.Kind != auth.CredentialAPIKey || id.Credential.ID != state.ID() ||
		!hasBound || bound.BoundScope() != a.cfg.AppScope || !hasChecker || !hasNative {
		return nil, auth.ErrForbidden
	}
	if _, err := uuid.Parse(state.ID()); err != nil {
		return nil, auth.ErrForbidden
	}
	if err := a.currentGroup(ctx, a.cfg.AppScope, "app"); err != nil {
		return nil, err
	}
	record := publishingCredential{Version: 1, Tenant: scope.TenantID.String(), App: scope.AppID.String(), Issuer: a.cfg.Issuer,
		Key: state.ID(), Actor: id.Subject}
	for _, permission := range []access.Permission{access.ContentWrite, access.EntitiesWrite} {
		name := AppPermission(permission)
		allowed, err := checker.Can(ctx, a.cfg.AppScope, name)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		p, err := native.Permission(name)
		if err != nil {
			return nil, publishingAuthError(err)
		}
		allowed, err = native.Can(ctx, iam.APIKeyIdentity(state.ID()), iam.GroupByID(a.cfg.AppScope.ID), p)
		if err != nil {
			return nil, publishingAuthError(err)
		}
		if allowed {
			record.Permissions = append(record.Permissions, name)
		}
	}
	if !slices.Contains(record.Permissions, AppPermission(access.ContentWrite)) {
		return nil, auth.ErrForbidden
	}
	return json.Marshal(record)
}

// ResumePublishing rechecks the same API key's current authority. The stored
// concrete permission list is an upper bound, so subsequent role expansion
// cannot expand an already accepted publishing operation.
func (a authorization) ResumePublishing(ctx context.Context, scope contentkit.Scope, raw json.RawMessage) (auth.Verified, error) {
	var record publishingCredential
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil || dec.Decode(new(any)) != io.EOF || scope != a.cfg.Scope || a.cfg.Issuer != a.cfg.PermissionAuthority ||
		record.Version != 1 || record.Tenant != scope.TenantID.String() || record.App != scope.AppID.String() ||
		record.Issuer != a.cfg.Issuer || record.Actor == "" {
		return nil, auth.ErrForbidden
	}
	if _, err := uuid.Parse(record.Key); err != nil {
		return nil, auth.ErrForbidden
	}
	native, ok := a.groups.(publishingPermissions)
	if !ok {
		return nil, auth.ErrForbidden
	}
	var ceiling []iam.Perm
	for _, name := range record.Permissions {
		if name != AppPermission(access.ContentWrite) && name != AppPermission(access.EntitiesWrite) {
			return nil, auth.ErrForbidden
		}
		permission, err := native.Permission(name)
		if err != nil {
			return nil, publishingAuthError(err)
		}
		ceiling = append(ceiling, permission)
	}
	id := iam.Within(iam.PinnedTo(iam.APIKeyIdentity(record.Key), a.cfg.AppScope.ID), ceiling...)
	id.Issuer, id.Subject, id.SubjectKind = record.Issuer, record.Actor, auth.SubjectApplication
	id.Invoker = auth.Invoker{Issuer: record.Issuer, ID: record.Actor}
	v := publishingVerified{id: id, scope: a.cfg.AppScope, native: native, permissions: slices.Clone(record.Permissions)}
	allowed, err := a.Authorize(ctx, scope, v, contentkit.PermissionTarget{Permission: access.ContentWrite.String()})
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, auth.ErrForbidden
	}
	return v, nil
}

type publishingVerified struct {
	id          auth.Identity
	scope       auth.Scope
	native      publishingPermissions
	permissions []string
}

func (v publishingVerified) Identity() auth.Identity { return v.id }
func (v publishingVerified) BoundScope() auth.Scope  { return v.scope }

func (v publishingVerified) Can(ctx context.Context, scope auth.Scope, name string) (bool, error) {
	if scope != v.scope || !slices.Contains(v.permissions, name) {
		return false, nil
	}
	p, err := v.native.Permission(name)
	if err != nil {
		return false, publishingAuthError(err)
	}
	allowed, err := v.native.Can(ctx, v.id, iam.GroupByID(scope.ID), p)
	return allowed, publishingAuthError(err)
}

func publishingAuthError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, iam.ErrInsufficientAuthority) || errors.Is(err, iam.ErrAPIKeyRevoked) ||
		errors.Is(err, iam.ErrAPIKeyExpired) || errors.Is(err, iam.ErrAPIKeyInvalid) {
		return auth.ErrForbidden
	}
	return errors.Join(auth.ErrUnavailable, err)
}
