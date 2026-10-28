---
status: accepted
date: 2026-10-14
---

# Share accounts within a tenant; isolate content by app

A ContentKit tenant is a business with one account directory and one linked
OpenRails merchant; it may own several apps. Accounts, profiles, avatars and
the inbox belong to the tenant, while catalogs, activity, content ownership,
roles, preferences, restrictions and access checks remain app-scoped. One
tenant registration works across its apps without a required app-membership
record.

This permits Doujins and Hentai0 to share the account experience they already
intend to share, while retaining separate content and authorization. A
per-app account directory would duplicate people and lifecycle work; treating
the two sites as one catalog would mix content and access rules. Tenant and
OpenRails merchant identities remain distinct and explicitly associated,
rather than assuming that matching IDs or names establish authority.

Storage prefixes, PostgreSQL schemas and deployment topology are derived
implementation choices, not additional account-sharing concepts exposed as a
public "namespace". `Config.Scope` is required, and its `AppID` is the catalog
key. See [the implementation plan](../architecture/tenant-app-accounts.md)
for the ownership model and verification gates.
