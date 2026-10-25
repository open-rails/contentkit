# Content-item entitlements

Each gallery edition is independently accessible. Create a product entitlement
with `access.VersionKey(appID, galleryID, versionID)` for that exact item.
The same subscription tier or channel membership can satisfy several items'
configured rules, but holding one version's purchase key never opens a sibling.

ContentKit asks OpenRails for the authoritative held keys. This adapter does
not copy the product catalog, grant access or migrate purchases. The gate and
purchased library match both the parent and version parts of a version key.
The gallery parent only groups the independently granted editions.

## Existing parent grants

`access.ContentKey` is retained as a parseable legacy identity for audit and
explicit migration. It is not used or offered by edition access decisions.
Do not create new products granting parent keys or retain a runtime fallback
that dynamically opens all current or future versions.

Before adopting the per-item gate, inventory product entitlements and existing
access grants in the linked OpenRails merchant. Convert each legacy parent
right to an explicit approved set of version keys. Preserve purchase and grant
identity, time bounds, revocation and the intended already-purchased editions.
An unresolved mapping must stop cutover; do not guess which editions a prior
purchase covers. A new edition does not inherit an old parent purchase.

Legacy key constructors, parsing and explicit `Gate.Holds` can read the old
identity for this audit. `Filter.Contents` is audit data only; readers and
libraries must use exact version grants. Migration is an explicit billing
operation owned by OpenRails, not a startup job in this adapter.
