# Authorization for tenant accounts and app content

`Config.Scope` fixes one canonical tenant and app. The same tenant UUID owns
its AuthKit accounts and inbox and ContentKit shared avatars; it is not an
issuer, schema name or billing merchant ID. Catalogs remain separate per app.
Scoped engines require `Deps.Authorization`; legacy engines retain their
existing host authorization contract until explicitly migrated.

The host supplies `Deps.Auth` to verify the original credential, including
issuer, audience, signature and live credential checks. `ResolveActor` returns
the source actor used for administration and audit. The optional
`TenantAccountAuthorization.ResolveTenantAccount` independently establishes an
existing audience account in the target tenant. A global operator acting in a
different tenant retains its source actor and has no implicit audience account.
It cannot acquire preferences, history, favorites or a self avatar there merely
by opening the administration UI. Existing custom policies without the new
interface retain their prior trusted actor-as-account contract; policies that
admit cross-tenant actors must implement it.

`access.Actor.ID` and `Verified.Identity()` retain the actor. `AccountID()` and
`Member()` answer audience membership; a nonnil empty `AccountMembership`
explicitly marks a nonmember. Nil membership preserves the legacy contract.
User routes require a tenant member. The internal Person tier permits a human
operator on permission-checked moderation routes without allowing application
credentials. Staff and Editor routes retain their own permission checks.

## AuthKit adapter

`adapters/authkit.NewAuthorization(ctx, client, config)` reads existing groups
and validates their ownership; it creates no tenant, group, account or grant.

- `Scope` is the exact canonical tenant/app pair.
- `Issuer` is the verified identity issuer.
- `PermissionAuthority` owns the permission groups and defaults to `Issuer`.
- `AppScope.ID` must equal `Scope.AppID`, with persona `app` and group ownership
  equal to `Scope.TenantID`.
- `TenantAdminScope`, when supplied, is the `tenant` group with that same
  canonical tenant UUID and ownership.
- `TenantActors` reads verified live source-account ownership, defaulting to
  the AuthKit client. It compares that ownership with the target for membership,
  without creating or looking up a target account by matching contact details.
- `Entities` optionally resolves an explicitly verified legacy content-entity
  binding. New applications use deterministic `AppScopes` IDs.

A dedicated legacy identity issuer may still supply `Accounts` with
`IssuerAccounts`. This explicitly trusts its entire account directory to one
tenant and must not be used with an issuer shared by unrelated tenants.
`Accounts` and `TenantActors` are mutually exclusive.

The adapter rechecks live group ownership during authenticated admission and
before group permission checks. Deleting a binding or recreating its ID under
another tenant does not retarget a running engine. Directory read failures
return an unavailable error; they do not invalidate the user's login.

The adapter uses ordinary `auth.PermissionChecker.Can`. A root owner inherits
all registered permissions within that authority. Other root roles grant only
what their definitions contain. AuthKit's tenant owner and explicit tenant
role grants inherit into groups belonging to that tenant only. App-bound and
tenant-bound credentials keep their native scope limits and permission
ceilings. A credential bound to the authority's verified root scope may carry
global operator permissions; arbitrary or foreign bindings are rejected.
An unbound application credential is never treated as a global operator.

Declare app operations once with `AppPermission`, such as
`app:comments:moderate`. A limited tenant role includes those same `iam.Perm`
objects; it does not define a duplicate `tenant:comments:moderate` permission.
`TenantPermission` returns a name only for shared account operations (currently
`tenant:avatars:write`), and an empty string for app-only operations. Declare
shared permissions through AuthKit's built-in `Roles.Tenant`; do not create a
second tenant persona. App permission checks use the app group, allowing
AuthKit to apply its own root/tenant inheritance. Avatar administration checks
the tenant group instead.

`Deps.Users` must be a tenant-bound directory, such as
`client.ForTenant(ctx, tenantID).UserInfo()`. Scoped privileged avatar edits
and user notification publications verify that their target exists there. A directory error remains an error;
it does not turn an unknown target into a member. Self editing requires the
resolved target-tenant account. Missing directory support fails closed for
scoped avatar edits and user notification publication. A publisher's own
administrative authority does not prove that a requested recipient is a member
of the target tenant. Native `Publish`, `PublishTx`, batch and HTTP publication
use the same recipient check before writing; accepted writes retain the durable
recipient-erasure fence.

For `auth_scope` content entities, `AppScopes(client, scope)` keeps the existing
deterministic UUID based on tenant, app and content-group ID. It checks the
provider group's tenant ownership and uses effective Can. The engine still
checks that every requested scope belongs to an active local content group.
Entity listings intersect directory grant candidates with the request's
verified permissions, so their editor flags also retain credential ceilings.
The app UI should request the ordinary effective permission view for its app
group; `direct=true` remains useful only for questions specifically about
direct assignments.

## Explicit migration

Existing `contentkit` and `contentkit-tenant` group personas are not silently
renamed at startup. The adapter returns `ErrScopeMigrationRequired`. Stop
incompatible writers and use an explicit AuthKit migration that preserves
group IDs, memberships, role definitions and credential scope/ceilings.
`Client.MigrateGroupPersona` accepts `iam.GroupPersonaMigration`: review its
read-only dry-run, then apply the same explicit role and concrete-permission
maps with `ExpectedFingerprint`. It takes a short maintenance table lock;
normal startup never performs this transition.
The required mappings are `contentkit` to `app`, and `contentkit-tenant` to
`tenant`; app-operation grants formerly prefixed `contentkit-tenant:` map to
`app:`, while its shared avatar grant maps to `tenant:avatars:write`.
Unknown/custom grants require an explicit mapping or refusal, not a guessed
expansion. Catalog, media, inbox and erasure IDs do not change.

In particular, old `contentkit-tenant:owner` is not automatically equivalent
to `tenant:owner`: the latter also administers tenant accounts and may cover
merchant operations. Preserve the translated old grant set in an explicit
limited compatibility role, and refuse any proposed mapping that adds grants.

Existing single-directory accounts are adopted into the explicitly chosen
canonical tenant through AuthKit's migration option, preserving account IDs.
A missing or mismatched ownership binding fails closed. Do not create a second
account directory, copy roles to real users, recreate groups or invent an
AuthKit-to-ContentKit tenant crosswalk as an adoption shortcut.

## Erasure and restore

Hosts pass no `OnPurge` callback. AuthKit purges the account first and records
`user.purged` in its retained event log. Each host's event reader calls
`AnonymizeAccount` and advances its cursor only after success. Failed cleanup
remains pending for retry after an outage or restart. Anonymization removes
account attribution while retaining comments and anonymous contributions.

Each deletion commits an app intent together with its `purging` lifecycle row,
even with notifications disabled. Intents survive completion. AuthKit's
`notify_recipients.erased_at` in the shared ContentKit scope also records a purge
obligation.

A content-only restore must preserve the shared account store. This app's
completion receipt and local `engine_cleanup_complete` fence commit in one
transaction. For ordinary purges, startup returns `ErrErasureReplayRequired`
when this app has a receipt but neither that fence nor lifecycle state
`purged` remains: evidence of a catalog restore. A pending purge without an
app intent or receipt can start so its event reader can finish. It never waits
for another app's receipt. A sibling may therefore start while the avatar primary is still
deleting the shared avatar file; its URL remains reachable until cleanup.
The `contentkit_account_anonymization` probe fails when a purge obligation older
than 15 minutes, within the last 30 days, still lacks this app's receipt. It does
not block startup.

An app intent with no local completion fence and no `purging` or `purged`
lifecycle row also requires replay. Successful `AnonymizeAccount` writes this
app's receipt.

The operator's replay tool opens `NewForErasureReplay`, calls
`ReplayErasureFences` until complete, closes it, and starts the normal engine.
The repair engine cannot serve routes or its in-process client, compose/start
jobs, or automatically migrate schemas. Replay repeats `AnonymizeAccount` for
every recorded deletion, preserving anonymous contributions and clearing restored
comment, creator and owner attribution, so restored data ends as live deletion
left it. This also resumes the app's avatar responsibility.
`ReplayAccountErasures` finishes purges that never completed, including
receipt-backed obligations without an inbox. Failed anonymization leaves the
report incomplete and returns the error; retry until complete.

To finish pending purges from an operator command, run
`tools replay-content-erasures --complete-accounts`. This invokes
`ReplayAccountErasures` before local replay, without starting workers or
serving. Ordinary purge retries belong to each host's event reader; no periodic
erasure sweep is required.

## Evidence and remaining work

Source tests exercise ordinary accounts, app-specific moderation, same-tenant
administration, global root grants, foreign issuers and bindings, credential
ceilings, live group ownership and revocation. These tests do not
establish that either existing host has migrated its live configuration or
that a released AuthKit/ContentKit cohort contains this interface.

The broader [tenant/app plan](tenant-app-accounts.md) still governs app-specific
restrictions, account erasure across all apps, restore safety, host adoption,
and release/deployment qualification. Do not close tracker #141 solely because
the authorization interface and adapter are available.
