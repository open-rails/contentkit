# OpenRails owns products, prices and purchased entitlements

OpenRails is the sole authority for products, prices, subscriptions, purchases
and entitlement grants. ContentKit owns content, versions, entities and the
access requirements that reference those entitlements. The former local offer
catalog and its synchronization workers duplicate billing authority and are
removed rather than renamed.

ContentKit's existing entitlement key identities remain stable. Products can
grant a content key, a version key, an entity/channel key, a tier key or several
keys. Authoring applications use OpenRails's product/price APIs and billing UI;
ContentKit evaluates the resulting grants through the same embedded or HTTP
entitlement reader. Product edits report OpenRails failures directly rather
than acknowledging a local price write for later synchronization.

The migration removes only ContentKit's redundant sale state. It must refuse
unsettled legacy writes and never delete or recreate OpenRails products,
prices, subscriptions, purchases or granted entitlements. Content deletion
stops ContentKit serving that content; it does not delete billing history or
silently cancel unrelated subscriptions.
