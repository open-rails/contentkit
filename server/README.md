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
    content_kinds:
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

`engine.scope` and `authorization` are required. The catalog key is the app ID;
account and media roots are derived from the canonical scope.

## Configuration

The file is YAML; unknown keys are refused. Durations are Go durations (`30s`).

| Key | Meaning | Default |
|---|---|---|
| `engine` | The embedded engine's `contentkit.Config` ([host integration](../HOST_INTEGRATION.md)). | required |
| `engine.trusted_proxies` | CIDR prefixes of the proxies in front of the server (an ingress, a load balancer). From one, the client is read from `Forwarded` or `X-Forwarded-For`, right to left past trusted hops; a malformed chain is 400. Behind a proxy, set it: otherwise every visitor is the proxy, location rules match nobody and anonymous visitors share one limit. See [client address](../HOST_INTEGRATION.md#client-address). AuthKit takes the same proxies, or the peer when there are none: `auth.http` declares no proxies of its own, and `auth.http.direct_peer_ip` beside a proxy, or AuthKit proxies that differ from these, refuse startup. | none: the peer is the client |
| `engine.clickhouse.tls`, `engine.clickhouse.ca_file` | Connect to ClickHouse over TLS, migrations included, verifying it against the system roots or the PEM CA file. | plain |
| `auth` | AuthKit as a resource server; its tables are in `auth.database.schema`, its River in `auth.database.river_schema`. Neither may be one of the engine's schemas. `auth.token.issuer` is https (http only on a loopback host). | `contentkit_auth`, `contentkit_auth_river` |
| `authorization` | The AuthKit groups and identity issuer the engine is bound to ([scoped authorization](#scoped-authorization)). | required |
| `users_issuer` | The issuer whose directory names authors; when set it must equal `authorization.issuer`. | `authorization.issuer` |
| `openrails.base_url` | The OpenRails server holding the site's merchant; https, or http only to a loopback host. Empty runs without billing. | none |
| `s3` | The media bucket: `bucket`, `region`, `endpoint`, `public_endpoint` (where browsers PUT presigned uploads), `use_path_style`. Both endpoints are http(s); plain http suits a private network, since requests carry SigV4 signatures, never the key. | `region` `us-east-1`, `public_endpoint` the `endpoint` |
| `routes` | Route groups served (`content`, `upload`, `media`, `codes`, `entities`, `discovery`, `import`, `all`). | every group the engine can serve |
| `prefix` | Where the API is served: `/api/contentkit` serves `/api/contentkit/v1/*`. | the root |
| `addr` | Listen address. | `:8080` |
| `drain_delay` | How long the server keeps serving after a stop signal with `/readyz` failing. | 0 |
| `shutdown_timeout` | Bound on finishing in-flight requests. | `30s` |
| `read_timeout` | Bound on reading a request's body, from when its headers are read. Answers, event streams and archive downloads are not bounded by it. | `60s` |
| `idle_timeout` | Bound on a kept-alive connection between requests. | `120s` |

Request headers must arrive within 10 seconds and fit 64 KiB (431 otherwise). Every answer carries `X-Content-Type-Options: nosniff`.

Secrets come only from the environment: `CONTENTKIT_MEDIA_TOKEN_KEY`, `CONTENTKIT_MEDIA_TOKEN_PREVIOUS_KEY`, `CONTENTKIT_S3_ACCESS_KEY_ID`, `CONTENTKIT_S3_SECRET_ACCESS_KEY`, `CONTENTKIT_CLICKHOUSE_PASSWORD`, `CONTENTKIT_OPENRAILS_API_KEY`.
