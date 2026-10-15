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
    tenant_id: a7f636e8-6303-40ee-a323-4d66e0f8f59e
    app_id: 2b8cbe6e-eb89-48e9-98d9-d298a82e2b20
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
  app_group_id: 2b8cbe6e-eb89-48e9-98d9-d298a82e2b20
  tenant_group_id: a7f636e8-6303-40ee-a323-4d66e0f8f59e
```

The IDs are examples. Generate deployment IDs once, persist them, and use the
same values for every restart. `app_group_id` must name a live AuthKit group
with the `contentkit` persona. The optional `tenant_group_id` must name a
different live group with the `contentkit-tenant` persona. Omitting it gives
the app no tenant administration fallback. Declared `contentkit:staff` and
`contentkit-tenant:staff` roles are available for explicit provisioning;
startup does not create groups, assign roles, or copy root grants.

`authorization.issuer` is the verified identity issuer for one dedicated
tenant account directory. The built-in account resolver preserves that
issuer's subject IDs. It is unsuitable when the same issuer serves unrelated
tenants without a trusted account-to-tenant mapping. `auth.token.issuer` is the
local permission authority; it may differ from the identity issuer.

For an external identity issuer, provision its enabled remote application in
`app_group_id` using AuthKit's group-scoped application API, and explicitly
grant the intended application role there. Register its public keys or JWKS
URL and restrict the token's permissions/scopes. `auth.remote_applications`
declares root applications and must not declare this scoped identity issuer.
The server refuses missing, disabled, root-only, or other-app registrations.
Remote user information is read from that same app group. Tokens from other
issuers, sibling app bindings, and root-only grants do not gain app authority.

When AuthKit also owns the local account directory, set
`authorization.issuer` equal to `auth.token.issuer`; no remote registration is
needed. Content group kinds with `auth_scope: true` use deterministic
tenant/app-specific AuthKit group IDs through `AppScopes`.

Legacy configurations without `engine.scope` retain the existing root
`staff` policy and trusted-root directory behavior. They must omit the
`authorization` section. Adopting existing data requires the engine's explicit
scope/account migration procedure; changing a namespace or regenerating IDs
is not an adoption strategy.
