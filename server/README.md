# Standalone ContentKit

The server runs ContentKit behind an AuthKit resource server. `engine` is the
same configuration accepted by the embedded engine; secrets come from the
environment as listed in `config.go`.

## Scoped authorization

Scoped deployments bind the catalog to a persisted tenant and app, and bind
authorization to existing AuthKit groups. For example:

```yaml
engine:
  scope:
    tenant_id: 11111111-1111-4111-8111-111111111111
    app_id: 22222222-2222-4222-8222-222222222222
  database:
    schema: example_catalog
  accounts:
    schema: example_accounts
  content:
    languages: [en]
    kinds:
      gallery: {route: g, presentation: reader, versions: {languages: one}}
auth:
  database:
    schema: example_auth
  token:
    issuer: https://contentkit.example.test
    issued_audiences: [contentkit]
  resource:
    id: https://contentkit.example.test
authorization:
  issuer: https://accounts.example.test
  app_group_id: 22222222-2222-4222-8222-222222222222
  tenant_group_id: 11111111-1111-4111-8111-111111111111
```

The IDs are examples. Generate them once and use the same canonical tenant
UUID in AuthKit and ContentKit. The server derives AuthKit's default tenant
from `engine.scope.tenant_id` when absent and rejects a conflicting explicit
value. Provision the tenant before startup. `app_group_id` must equal the app
UUID and name a live `app` group owned by that tenant. The optional
`tenant_group_id` must equal the tenant UUID and name its built-in `tenant`
group. Startup does not create groups or assign roles.

Local accounts use AuthKit's live tenant ownership. Root operators may
administer other tenants with native effective permissions while retaining
their source identity; this does not create an audience account there.
`app:staff` grants apply to the app, and `tenant:staff` uses the same app
permission objects across that tenant's apps. Shared avatar administration is
`tenant:avatars:write`. A tenant-bound user directory validates avatar targets.

`authorization.issuer` is the verified identity issuer;
`auth.token.issuer` is the local permission authority. For an external identity
issuer, provision its enabled remote application in `app_group_id`, with
explicitly restricted permissions and public verification keys. This remote
compatibility path requires a dedicated one-tenant issuer; it cannot attest
ownership for a shared multi-tenant account directory. Local shared AuthKit
setups use the native verified tenant resolver instead.

Existing `contentkit` and `contentkit-tenant` personas require an explicit
AuthKit migration preserving IDs, assignments and credential bounds before
startup; see [the authorization contract](../docs/architecture/app-authorization.md).

Legacy configurations without `engine.scope` retain the existing root
`staff` policy and trusted-root directory behavior. They must omit the
`authorization` section. Adopting existing data requires the engine's explicit
scope/account migration procedure; changing a namespace or regenerating IDs
is not an adoption strategy.
