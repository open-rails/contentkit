# Authorization for tenant accounts and app content

`Config.Scope` selects a tenant and app; it does not prove that the caller may
use either. A scoped engine therefore requires `Deps.Authorization`. Legacy
engines keep their existing root permission behavior until explicitly migrated.

The host still supplies `Deps.Auth`, which verifies the request once, including
the identity provider's live account and session checks. ContentKit then calls
`Authorization.ResolveActor` with the fixed scope and original verified
credential. The returned actor ID is the tenant account ID for a person or the
scoped service identity for an application. The signed issuer and subject stay
unchanged in `Actor.Verified.Identity()`; contact details never establish an
account association.

Ordinary signed-in use requires an account in the tenant, not a membership or
role in every app. Staff and group-editor checks call `Authorization.Authorize`
with a `PermissionTarget`. An empty `GroupID` means an app staff operation; a
nonempty value identifies a stored content-group authorization scope. The
provider must verify that group belongs to this app. Ownership checks use the
mapped tenant account ID and app-owned content records.

## AuthKit adapter

`adapters/authkit.NewAuthorization(ctx, client, config)` accepts a trusted
`AuthorizationConfig` and checks that the live AuthKit groups have the expected
app/tenant personas and permission authority, refusing the deployment root as
an app group:

- `Scope`: the exact ContentKit tenant/app pair this adapter serves.
- `Issuer`: the verified account issuer.
- `PermissionAuthority`: the authority of the local AuthKit permission groups;
  it defaults to `Issuer`. A resource server with an upstream account issuer
  sets both explicitly instead of conflating identity and permission ownership.
- `AppScope`: the existing AuthKit permission group bound to this app.
- `TenantAdminScope`: an optional, distinct group explicitly administering the
  tenant; this is never inferred from AuthKit's deployment root.
- `Accounts`: verified issuer/subject-to-tenant-account mapping.
- `Groups`: an optional override for an explicitly verified legacy group
  binding. New apps omit it and use deterministic app-scoped group IDs.

`IssuerAccounts` is a convenience for an issuer whose whole account directory
belongs to one tenant and whose subject IDs are the existing account IDs.
It is inappropriate when one issuer holds accounts for several tenants or IDs
need translation; those deployments supply an explicit resolver.

Separating the two authorities does not relax either check: the verified
identity must still match `Issuer`, while app, tenant and content-group scopes
must belong to `PermissionAuthority`. The original credential's live grants,
ceilings and bound scope continue to be enforced.

The adapter checks exact grants using AuthKit's additive
`auth.DirectPermissionChecker.CanDirect` capability. Existing `Can` deliberately
inherits root grants and retains that behavior. `CanDirect` retains live
account/session, credential ceiling and bound-scope checks, while requiring a
grant in the requested group itself. A credential without the new capability
cannot obtain privileged access through this adapter.

Declare `AppPermission(access.CommentsModerate)`, for example, in the
`contentkit` persona and assign its role only to the relevant app group.
`TenantPermission` uses the `contentkit-tenant` persona and is assigned only in
the explicitly configured tenant administration group. Shared avatar
administration (`AvatarsWrite`) requires this tenant scope; an app moderator
cannot edit shared account avatars merely by holding an app role. Self avatar
editing uses the mapped account ID, as before.

An unbound user session can use the account across the tenant's apps. An app
service credential must be bound to the configured app or explicit tenant
administration scope. Credentials bound to other scopes, including a content
group, are currently refused at app admission; creator-group roles work with
an ordinary user session. Supporting group-bound credentials later requires
an explicit admission contract, not a permissive fallback.

The adapter does not provision groups, assign roles, merge accounts, implement
cross-origin SSO or infer app ownership from an arbitrary UUID. Provisioning
must register and verify the mapping before constructing the engine.

For content kinds that enable `auth_scope`, configure `AppScopes(client, scope)`
as the engine's group-scope provider. It derives the AuthKit group UUID from
the tenant, app and stored content-group scope ID. Equal content-group IDs in
different apps or tenants therefore have different authority. The engine
checks the provider's binding at startup, verifies that a requested scope is
an active local content group, and supplies only local group IDs to grant
lookups. Legacy unscoped engines retain `Scopes(client)`; switching existing
group grants requires explicit adoption, never root-role promotion.

An app UI requests `/me/permissions?group_id=...&direct=true` (or
`getPermissions({groupId, direct:true})` / `usePermissions({groupId, direct:true})`)
to receive the same direct-grant view. The older default remains the identity
provider's inherited view and must not drive app administration controls.

Host migration must explicitly assign the intended app roles and tenant
administration roles, then issue service credentials bound to those groups.
In particular, an existing avatar forwarding key holding
`root:avatars:write` does not automatically become a tenant avatar key: issue
an equivalent key in the tenant administration group with
`contentkit-tenant:avatars:write`. Retain the old configuration until both
forwarding and primary services can use the new binding together. Do not make
the old root role a silent fallback when switching on `Config.Scope`.

## Erasure and restore

`EraseSubjects` erases the shared account resources plus this engine's local
activity. AuthKit's configured `Token.AccountIssuers` and durable `OnPurge`
deliveries invoke that work for every tenant app and prevent final account
purge while any app still has an unfinished obligation. A failed callback
remains retryable after restart. ContentKit does not duplicate that coordinator.

`EraseAppSubjects` is a privileged app-only erasure boundary. It permanently
fences local activity and app notices while preserving the tenant inbox,
avatars and sibling apps. This is not a new account-registration or user
reset flow. Shared notification tombstones retain both whole-account and
app-only erasures, and database guards fence older delayed publishers.

A content-only restore must preserve the shared account store. Normal scoped
startup refuses `ErrErasureReplayRequired` when shared tombstones are missing
their local content fences or completed cleanup receipt. Full account cleanup
has a separate receipt in the preserved account schema, keyed by tenant, app
and actor. Only successful `EraseSubjects` writes it; app-only cleanup never
does. A global tombstone requires both this app's receipt and the avatar
primary's receipt, so a secondary cannot discharge failed shared media cleanup.
A source SQL commit alone does not complete erasure while another plane failed.
The operator's replay tool opens
`NewForErasureReplay`, calls `ReplayErasureFences` until complete, closes it,
and starts the normal engine. The repair engine cannot serve routes or its
in-process client, compose/start jobs, or automatically migrate schemas.
Replay erases restored app data without erasing shared avatars or receipts.
If full account cleanup has not finished (including historical global erasures
without a receipt), explicitly run `tools replay-content-erasures --complete-accounts`
on the avatar primary first and then the other apps. This invokes
`ReplayAccountErasures` before local replay; it retries the full obligation,
including avatar cleanup, without starting background workers or serving.
AuthKit's durable purge fanout remains the ordinary retry coordinator.

## Evidence and remaining work

Source tests exercise a shared ordinary account, app-specific moderation,
explicit tenant administration, foreign issuers, foreign credential bindings,
root-inheritance denial, ceilings and session revocation. These tests do not
establish that either existing host has migrated its live configuration or
that a released AuthKit/ContentKit cohort contains this interface.

The broader [tenant/app plan](tenant-app-accounts.md) still governs app-specific
restrictions, account erasure across all apps, restore safety, host adoption,
and release/deployment qualification. Do not close tracker #141 solely because
the authorization interface and adapter are available.
