# Tenant accounts and app catalogs

Status: accepted ownership model, phased implementation. Updated 2026-10-14.
This document specifies the target; the source inventory below distinguishes
it from the working tree's existing and in-progress implementation.

The owner approved one tenant account shared by Doujins and Hentai0, separate
catalogs, shared avatars and inboxes, app-specific notification filtering,
and notifications for billing outcomes that happen outside the user's direct
action. [ADR-0001](../adr/0001-tenant-accounts-and-app-catalogs.md) records the
ownership choice. [GLOSSARY.md](../../GLOSSARY.md) defines the product terms.

## Ownership model

```text
ContentKit tenant (business)
  ├── one linked OpenRails merchant
  ├── accounts → profile, avatar, shared inbox
  ├── app: Doujins → catalog, comments, activity, roles, preferences, restrictions
  └── app: Hentai0 → catalog, comments, activity, roles, preferences, restrictions
```

The ContentKit tenant and OpenRails merchant have separate stable IDs with a
verified one-to-one binding. The binding may be absent before billing is
configured; once configured, apps of that tenant use the same merchant.
Existing account IDs are preserved. Equal email addresses, usernames, slugs
or arbitrary UUID values do not prove an association across tenants or
between an OpenRails customer and a ContentKit account.

| Record or operation | Owner and scope | Consequence |
| --- | --- | --- |
| Registration, profile, avatar | Tenant and account | Editing an avatar in either app changes the same avatar |
| Credential, session, recovery, tenant account disablement | Tenant's identity provider | One registration works across all apps; sessions still obey origin/audience rules |
| Inbox receipt and read/archive state | Tenant and account | Reading a tenant-wide billing notice in one app reads it in the other |
| Catalog, versions, media, taxonomy, comments, reactions, favorites, reading history | Tenant and app | App queries, counters, URLs and access rules stay separate |
| Moderator/editor roles, app preferences and restrictions | Tenant, app and account | A Doujins moderator or ban is not automatically a Hentai0 moderator or ban |
| Content ownership | Tenant, app, content and account | One account may own content in both apps, with separate authority |
| Product, charge, subscription, retry decision, access grant | Linked OpenRails merchant | OpenRails remains the authority; ContentKit presents the outcome and enforces access |
| App entitlement mapping | Tenant and app | One merchant product may grant one or several apps' entitlements explicitly |

There is no required `app_membership` record. An account can visit an app
with no app-specific row and receive its default settings and ordinary
signed-in behavior. Create rows only for actual facts, such as a preference,
role, restriction or activity. Entitlements are also facts, not registration.

Tenant profile fields and app preferences must be declared deliberately.
Avatar and the shared account profile are tenant-owned. Content filters,
reading preferences and moderation settings are app-owned. A cross-app
preference may be added explicitly; a generic preferences bag must not silently
promote app data into tenant data.

## Current source and migration constraints

Inspected on local `master` at base `4dc7fb5c63f30437704b87906348a764e779c4d0`,
including uncommitted work on 2026-10-14. This is source evidence, not a claim
that a release or deployment contains all of it.

| Source | Current behavior | Target implication |
| --- | --- | --- |
| [`config.go`](../../config.go), [`engine.go`](../../engine.go) | `Config.Tenant` pins a catalog, access gate, signal scope and media setup | Its current value is a legacy **app** key, not the new business tenant |
| [`scope.go`](../../scope.go) | Local typed tenant/app scope and immutable app/account-storage claims | Establishes compatible ownership metadata; it does not grant app permissions |
| [`accounts.go`](../../accounts.go) | Scoped `AccountsConfig{Schema, Avatars}` derives the tenant account root; legacy `Namespace` remains a migration input | Shared account/avatar coordinates do not configure an inbox |
| [`account_erasure_receipts.go`](../../account_erasure_receipts.go), [`migration/legacyinbox`](../../migration/legacyinbox) | Existing account-erasure fences and explicit legacy receipt adoption use AuthKit-owned storage | Normal notification publication uses only AuthKit's publisher; ContentKit has no inbox reader or fallback store |
| [`account-notifications.md`](../account-notifications.md), [`notification/openrails`](../../notification/openrails) | AuthKit-owned inbox with an injected transactional publisher; the former billing inbox poller is removed and its adapter remains only for explicit legacy handoff | Keep general event logs independent; OpenRails owns selected billing notices and their resolution |
| [`avatars.go`](../../avatars.go), [`media/avatar`](../../media/avatar) | One avatar writer; secondary app forwards authenticated self-only edits | Keep one canonical manifest and processing flow |
| [`discovery/stats.go`](../../discovery/stats.go), [`adapters/openrails/tenure.go`](../../adapters/openrails/tenure.go) | App comments/completions plus a thin adapter to OpenRails' native access-tenure query | Keep counters app-scoped; expose billing-derived tenure through ContentKit |
| [`media/layout/layout.go`](../../media/layout/layout.go) | Scoped paths include the tenant and optional app; legacy paths remain explicit compatibility inputs | Registry, tokens, gateway, workers and verified copy plans share the same scope |
| [`access/keys.go`](../../access/keys.go), [`content/runtime.go`](../../content/runtime.go) | Legacy tenant values occur in durable keys, references and query predicates | A config rename must not rewrite existing catalog/access identities |
| [`internal/gate/gate.go`](../../internal/gate/gate.go), [`adapters/authkit/authorization.go`](../../adapters/authkit/authorization.go) | Legacy staff checks retain root scope; scoped engines require the app authorization port and explicit AuthKit grants | Host role/key adoption remains separate from the implemented adapter |

The existing [design notes](../DESIGN.md) use "tenant" for the catalog
isolation key. That accurately describes the current implementation, but
this decision supersedes that term for the public ownership model. Do not
silently reinterpret old fields or claim the new isolation is complete after
only renaming them.

## Public configuration and identity

The target public context is exactly a tenant and one of its apps. IDs are
opaque, stable typed IDs, represented as UUIDs in new provisioning. Slugs
such as `doujins` and `hentai0` are readable names for display/routing, with
tenant-scoped app uniqueness; they are not foreign keys or authorization.

The in-progress embedded interface uses typed IDs and an explicit migration
key. This is source work, not a published-version claim:

```go
cfg.Scope = &contentkit.Scope{
    TenantID: tenantID, // same business in both apps
    AppID:    appID,    // different for Doujins and Hentai0
}
cfg.LegacyCatalogKey = "doujins" // existing catalog only; omit for a new app
```

`ParseTenantID` and `ParseAppID` accept persisted canonical UUIDs. `NewTenantID`
and `NewAppID` are provisioning helpers; never call them on every startup.
`Config.Tenant` remains the legacy-only spelling. Combining it with `Scope`
is rejected instead of silently deciding what "tenant" meant. Adopting an
existing app explicitly moves its old `Tenant` value to `LegacyCatalogKey`.
Omitting that key uses the new app ID for a new catalog; it does not discover
or migrate an older catalog automatically.

The first scope slice records immutable app-to-tenant/catalog bindings in the
catalog schema and account-storage-to-tenant bindings in the effective account
schema. An account namespace belongs to one tenant, and one tenant uses one
namespace within that account schema. App and account claims commit together;
replicas serialize claims and database constraints reject collisions. A
current-source legacy-only runtime refuses to open a claimed catalog or account
store. Across separate databases/account schemas, consistent tenant identity
still depends on trusted deployment configuration or the future hosted registry.
This metadata does not implement issuer validation, app roles or hosted
provisioning authority by itself.

Scoped runtimes also require an explicit `Deps.Authorization` binding. Its
`ResolveActor` maps the original verified credential to a tenant account or
scoped service identity, and `Authorize` checks a permission in the fixed app
and optional content group. A missing binding fails startup. The binding must
preserve credential ceilings, revocation and app restrictions; changing an
AuthKit scope string alone is not sufficient because provider-wide root
grants may be inherited. Legacy-only runtimes retain their existing authority
contract. The local AuthKit adapter uses exact-group `CanDirect` checks and
verifies its configured group personas; published delivery and host role/key
adoption remain #141 gates.

`Scope` is fixed when the embedded runtime or app-bound client is constructed.
The default user-facing routes remain app-bound (`/me/stats`, notification
inbox and media operations); clients do not need to repeat business IDs in
every request. A future hosted control plane may route by
`/tenants/{tenantID}/apps/{appID}`, but it must authenticate and bind that pair
before calling the same operations.

Provisioning or trusted embedded configuration resolves the context to:

- the tenant's account directory, profile/avatar store and inbox store;
- the app's catalog and activity stores and app authority;
- the linked merchant and verified customer-to-account mapping;
- existing compatibility identifiers where applicable.

Schema names, bucket names, old catalog keys and storage paths belong to
deployment/migration configuration, not an extra public "account namespace".
Embedded deployments still need to configure PostgreSQL and object storage;
the point is that these do not decide whose records may be shared. No service
discovery system or new process is required for embedded use.

New APIs must reject zero IDs, a pair whose app belongs to another tenant,
and conflicting new/legacy declarations. Trusted initialization must verify
the configured pair and bindings; naming an existing app is not authority
to access it. Avoid probing tenant/app existence in unauthenticated errors.

## Identity and authorization

The identity provider remains the source of authentication and tenant account
lifecycle. ContentKit stores references to that identity and shared profile
data; it must not create a second credential database. Identity associations
include the trusted issuer and tenant, even when the current installations use
the same subject UUID for AuthKit and OpenRails.

One registration is valid across tenant apps. This does not make a browser
cookie valid on every domain: each app performs a normal sign-in/SSO flow,
verifies the correct issuer and audience, and resolves the same tenant account.
An account at another tenant with the same email is a different registration.

App authority is checked on every app operation. Staff checks must resolve
permissions for `(tenant, app, account)`, or an explicit tenant-administrator
role authorized to manage that app. The current authenticator root permission
must not be copied into all apps by a scope rename. A blocked app action does
not disable the shared avatar or another app unless the restriction is
explicitly tenant-wide.

Content references, SQL keys and joins, cache keys, pagination cursors,
idempotency keys, rate-limit keys, background jobs, indexes, signal projections
and media capabilities must contain or resolve the same app context. An
account ID never implies app ownership or paid access. Shared resource writes
derive the tenant account from verified identity; avatar upload/editor routes
do not accept a browser-selected victim account or tenant.

## Shared inbox and billing lifecycle

The tenant has one inbox store and one receipt per `(account, source event)`.
Each notification also has explicit visibility:

- **Tenant-wide**: account-level events such as the linked merchant's billing
  outcome. Visible in every authorized app of that tenant.
- **App-specific**: replies, likes and other events concerning one app.
  Visible only through that app's view of the inbox.

Origin provenance and visibility are distinct concepts. A producer's
`Origin{System, Scope, ID, Subject}` identifies where an event came from;
it must not be the long-term substitute for declaring its tenant/app audience.
The existing origin allowlist can implement the compatibility view, provided
list, unread, pagination, get, read and archive all enforce the same filter.
Clients cannot change the allowed origins or request another app's feed.

All shared notices have common receipt state. An app-specific event may
contain only safe references; rendering and following them recheck current
app/content access. Tenant-wide billing notices should use a same-app
authorized billing action, not reveal another app's private content.

OpenRails owns the event facts and retry decision. ContentKit owns selection,
durable import, inbox rendering and reusable UI. Hosts provide identity and
verified merchant association plus authorized action destinations; they do
not each write billing filters, polling jobs, retry wording or duplicate
receipt stores.

| Outcome | Inbox policy | Required guidance |
| --- | --- | --- |
| Signup, successful checkout, routine renewal, direct upgrade | No automatic notice | The initiating flow gives its result |
| User cancels automatic renewal but paid access remains | No cancellation notice | Notify only when access actually ends |
| Automatic collection fails | Notify | Explain whether retry is scheduled, blocked pending user action, stopped, or genuinely unknown; provide the applicable action |
| Access expires after cancellation or failed collection | Notify when it actually ends | Explain ended access and applicable manage/resubscribe action |
| Access is revoked | Notify when effective | Explain lost access using safe user-visible information |
| Queued downgrade is applied | Notify when effective | Identify the applied change without suggesting it was only scheduled |
| Direct user checkout/change fails immediately | No duplicate default inbox notice | The initiating flow must report the failure and recovery instruction |

Payment guidance is a structured OpenRails event contract: stable event ID,
merchant/customer, failure category safe for the customer, retry status,
`next_attempt_at` only when scheduled, and recovery action. The action enum
supports updating a payment method, completing authentication, contacting
support, or opening billing management. Card/cardholder/provider secrets and
raw diagnostic text never enter the notification payload.

The notification records what was true when it occurred. Rendering an old
failure must not promise a retry that is no longer scheduled or claim the
problem remains unresolved; action pages fetch current authoritative billing
state. Do not infer `stopped` from a missing next-attempt timestamp. If source
facts are missing, label the guidance unknown and offer billing management.

Use one stable tenant destination and source-consumer identity across app
replicas. Publish before acknowledging the OpenRails event. Retry publication
and ACK safely after crashes, without changing existing read/archive state.
Two app processes may host workers for the same shared queue, but neither
creates a second logical destination. Queue/storage failures remain pending.
Deduplication is by stable source identity and recipient, not by human wording
or a time window: two real failed attempts can be different events.

## Shared avatars and storage

Avatars have one tenant-account manifest, source and processed renditions.
Either app can upload/edit through ContentKit UI and the same authenticated
operation. One writer performs processing and removal. The current primary
plus secondary-forwarding implementation is a valid transition; it should be
declared as shared infrastructure, not make Doujins conceptually own Hentai0
accounts. Any app endpoint receiving the operation validates the same tenant
account before forwarding.

One bucket can hold all of a tenant's resources with derived paths. A future
hosted deployment can also place multiple tenants in a bucket. A path prefix
does not enforce isolation; signed capabilities, authenticated reads and
gateway checks must validate the tenant/app resource context.

Logical target layout:

```text
tenant/{tenantID}/account/user/{accountID}/{manifest|private|public|temp}/...
tenant/{tenantID}/app/{appID}/{kind}/{contentID}/{versionID}/{area}/...
```

`versionID` is present for versioned content; the avatar's manifest owns its
generations without pretending an account is an app catalog item. The
`private`, `public`, `download` and `temp` access classes keep their existing
meaning. A profile/avatar ID can repeat in another tenant safely. Two apps can
have the same content ID safely. Public avatars remain intentionally public;
editor views and originals keep their existing access controls.

The logical target is not a request to rename live objects immediately.
Current storage uses `accounts/user/...`, `doujins/...` and `hentai0/...`;
`media/layout.Parse` accepts both shapes. Explicit copy plans hash and verify
objects and adopt their manifest/journal state before cutover. A scoped-only
gateway stops serving preserved legacy objects. The temporary `LegacyLayout`
mode requires a successful rollback check; it is never an automatic fallback.
See [media migration](../media-layout-migration.md).

Consolidating buckets is independent of renaming keys. First copy and verify
the existing keys into the shared bucket, switch explicit mappings, and keep
the source buckets. Counts alone are insufficient: verify object size and
content checksum plus representative manifests, private reads and public
renditions. Do not remove originals as part of this task. A later key migration
uses an explicit plan and read compatibility, with a single canonical write
location and resumable verification. Do not dual-write independent avatar
manifests.

## Statistics and lifecycle

ContentKit owns the reusable signed-in account statistics interface and UI.
Default `/me/stats` is an **app** view: published comment count, received
likes/dislikes, completed content by kind, and applicable access tenure. It
does not sum Hentai0 activity into Doujins activity or merge unrelated content
kinds with the same name.

Access tenure comes from OpenRails authoritative access windows, not account
creation time, subscription count or the first payment. Intersect each window
with elapsed time, clamp at effective expiry/revocation, and merge overlapping
windows before summing. Keep the machine value in seconds; the UI can render
months or years consistently. If history is unavailable, omit/mark unavailable
instead of displaying invented zero tenure. Legacy product-key changes require
explicit mapping before promising a complete historical total.


The approved initial `/account` display is exactly four metrics: Premium for
(OpenRails query; elapsed days below one month, otherwise months), Galleries
read (ContentKit completed-gallery count), Comments (ContentKit), and Likes
received (ContentKit). Dislikes may remain queryable but are not displayed in
this panel. Apps display these results and do not calculate tenure themselves.

A future tenant-wide account overview may request an explicitly authorized
aggregate with per-app breakdown. It is separate from the app default and
must avoid counting an entitlement or content event twice.

Tenant account erasure fences the shared inbox and avatar first and dispatches
app-owned deletion obligations to **all** tenant apps. Each obligation is
idempotent and remains pending until completed; a failure in Hentai0 cannot be
hidden by success in Doujins. App-only deletion/restriction affects that app's
records and never accidentally calls shared account erasure. `AnonymizeAccount`
deletes the account in the current app, with AuthKit's durable per-issuer purge
callbacks delivering the other app obligations before final identity deletion.

A catalog restored from backup must pass `ReplayErasureFences` before serving.
`NewForErasureReplay` opens a restricted recovery engine with no serving or worker
capability. Ordinary startup returns `ErrErasureReplayRequired` until each
replayed fence has its completed engine-cleanup receipt (main migration0005).
Avatar completion requires synchronous deletion and verification plus a durable
access fence; recorded late-upload cleanup continues after completion.

## Local database placement and startup

The local Doujins and Hentai0 stack uses one PostgreSQL database,
`doujins_db`. Its schemas separate app records (`doujins`, `hentai0`) and
catalogs (`doujins_contentkit`, `hentai0_contentkit`); `profiles`, `billing`
and `account_notifications` hold the shared account, merchant and inbox
records. A schema is a named section inside a PostgreSQL database.

ContentKit activity uses a separate database engine, ClickHouse. Both apps
use its existing `hub` database and shared tables. The tables' legacy
`tenant` column contains the app catalog key, so one app's reading activity
does not become the other's. `hub` is a historical storage name, not an
additional product concept or a separate service. ClickHouse does not use
PostgreSQL schemas; these two kinds of placement must not be conflated.

The owner specified an explicit migration boundary on 2026-10-14:
`task up` starts the Compose dependencies; `task dev` compiles and runs
migrations against those running services before starting Air and Vite.
Server and worker startup validate the required schemas without applying
migrations. This includes embedded AuthKit, OpenRails and ContentKit, their
River schemas, the AuthKit-owned shared inbox, and ContentKit's ClickHouse signal tables.
Missing migrations must produce an actionable error, not an automatic
upgrade. Explicit migration commands remain available outside development.
Local library compatibility modes and both host integrations implement this
boundary with `Database.SkipMigrations` and explicit migration entrypoints.
Real PostgreSQL tests cover unprepared startup, unchanged migration ledgers,
and prepared startup with DDL blocked. Both local migration commands completed;
published package and deployment evidence remain separate.

## Delivery plan and evidence gates

Tracker issues are the delivery authority; the phase headings below are
stable work packages to link from those issues. Every stage preserves current
local UI behavior and keeps existing data readable. Tests use at least two
tenants with two apps each; a one-tenant/two-app fixture cannot detect every
cross-tenant leak.

The [tracking plan (#139)](https://github.com/open-rails/tracker/blob/master/contentkit/0139-tenant-app-accounts.md)
links the implementation work: #140 context/configuration, #141 account
identity and app authority, #142 inbox and billing lifecycle, #143 media and
avatars, and #144 statistics and integrated host qualification. Hosted
provisioning is deferred in #145. These links describe the plan; local source
edits do not by themselves establish that an issue is delivered or released.

### A. Context and compatibility

Introduce typed tenant/app context, registered app-to-tenant and
merchant-to-tenant bindings, explicit account identity mapping, and validation.
Adapt existing catalog keys and account sharing fields internally. Keep the
old exported fields for a documented deprecation period and reject ambiguous
configuration. No automatic SQL-key, access-key or object rename occurs here.
The initial #140 slice establishes scope/configuration and durable app/account
storage claims; merchant identity, verified account mapping and app authority
remain separate delivery gates. It deliberately keeps the existing
`Accounts.Namespace` compatibility setting until #142 supplies the complete
shared-account configuration surface.

Gate: old configs read existing fixtures without changed keys; new configs
resolve two app contexts to one account store and separate catalogs; conflicting
bindings and cross-tenant pairs fail. Record exact source/version and test
commands before describing a release as ready.

### B. Tenant resources and billing integration

Finish shared inbox, avatars and account statistics using the current reusable
pieces. Introduce explicit notification visibility alongside origin provenance;
retain the origin allowlist for existing records. Bind one durable billing
destination per tenant and reuse ContentKit's billing UI in both hosts.

Gate: shared billing receipt IDs and read/archive state on both apps; private
app notices absent from every other app operation; duplicate delivery/crash
replay cannot duplicate receipts; scheduled/no-retry/user-action-required
guidance verified against typed source events; no signup/cancel notice;
expiry/revocation/downgrade each appear only when effective. Avatar changes from
either app appear in the other and reject victim/cross-tenant edits. Stats
remain app-local and tenure is authoritative.

### C. App authority and complete scope isolation

Bind roles, preferences, restrictions, ownership, entitlement mappings and
all new durable references to tenant/app context. Audit root-auth assumptions,
cursors, caches, signals, media worker jobs, erase flows and source deduplication.
Avoid adding empty per-app account or membership rows.

Gate: a role/ban in one app cannot affect another; identical content IDs and
subject IDs in unrelated scopes cannot collide; private signed media and
foreign cursors are refused; tenant erasure reaches every app and delayed
events cannot recreate shared records. Existing entitlements still resolve
under their exact legacy keys until explicitly migrated.

### D. Storage consolidation and optional layout migration

Complete the approved local shared-bucket copy/verification/mapping cutover
while preserving source buckets. Separately implement versioned parsing and
explicit migration to tenant/app-derived keys only when all readers and workers
support it. Keep checkpoint and rollback manifests outside disposable temp
state. Source removal is a separate owner decision.

Gate: verified checksums and manifest references, authenticated/private/public
and default-media reads, upload/edit/delete processing, worker restart and
rollback to legacy mappings. A healthy container alone is not media evidence.

### E. Hosted wrapper, when productized

Use the same tenant/app model and domain operations in an optional
`contentkit-saas` wrapper. The wrapper owns provisioning, authenticated app
credentials, routing, deployment/storage selection, quotas, export and
operations. The ContentKit libraries keep content/account-media behavior and
equivalent embedded/remote operations. Do not add microservices, a second
identity directory or a second notification pipeline to anticipate hosting.

Gate: a real authenticated remote catalog-ingestion/read flow and paid-access
flow match the embedded contract, including replay, erasure, export and
cross-tenant denial. A design or working embedded installation is not evidence
that a hosted SaaS product exists.

## Rollout and unresolved implementation choices

Register the current shared business and its two apps without changing account
IDs. Back up and inspect existing catalog, inbox, avatar and authority records.
Deploy readers that understand old and new scope metadata before new writers;
then migrate declarations and verified records in bounded, resumable batches.
Never merge by matching email. Start writers only when their destination and
consumer identities agree. Validate both app UIs and worker paths before
changing production configuration; source completion, local development,
published packages and production are separate evidence levels.
Older released writers do not know the new binding tables and cannot enforce
these claims. Quiesce every old writer/worker before adopting the new scoped
configuration; a successfully bound new process does not fence a previously
running old binary. Downgrade requires an explicit compatible cutover, not
simply restarting an old version against claimed storage.

The ownership decisions are settled. These mechanical choices remain bounded
implementation decisions, not reasons to reopen shared registration:

- The exact exported Go type names may evolve while retaining one explicit
  tenant/app pair and checked bindings.
- App authority can be supplied by AuthKit scopes or a host adapter, but must
  pass the same isolation contract and avoid broad root-role inheritance.
- New explicit notification visibility can live in the helper store or a
  ContentKit projection; it must coexist with legacy origins without leaking
  them or splitting receipt state.
- Profile field inventory and supported historical product aliases must be
  audited before migration; unavailable history is reported honestly.

Do not mark the target complete on the strength of `AccountsConfig` alone.
It solves useful current sharing, while the tenant/app identity, app authority,
full scope audit and safe key migration have their own gates.
