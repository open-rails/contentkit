# Search attribution

Search results, content suggestions and existing home/release/similar lists use the existing exposure, click and
measured-engagement path. Each search carries a query ID and normalized query
text; each rendered/visible list carries its render ID, language, ranking
identifier and absolute result positions. Numbered pages include the page
offset. Hidden cards are not reported as visible. Non-search lists retain their
surface and positions through the same bridge, with no search query attached.
A successful full search with no returned content records one rendered search
exposure with an empty result list. Loading, failed requests and cards hidden by
the presentation do not create zero-result observations. These rows retain the
query for search-quality counts even when there is no result to click; this is
not a raw request log or a record of every typed suggestion prefix.

Search links carry only an opaque `ck_nav` identifier. The SDK keeps its short
handoff in browser storage, scoped to the ContentKit mount, current identity and
target content. Ordinary, modifier, middle-click and context-menu navigation
renew that identifier. Activation records the click through the existing beacon
batcher; the destination supplies it only when native navigation bypassed that
event, then starts a separate visit identity. Query text is
never added to destination URLs. Blocked browser storage or lost telemetry
does not prevent navigation or reading.

The reader retains `origin_click_id` across its visit's checkpoints and edition
selection. Direct visits have no origin. Canonical engagement receipts keep
their existing revisions and access checks; origin is analytics context, not
authorization. The attribution export joins click IDs only within the same
tenant, subject and content. Its visits report observed active time, coverage
and completion from canonical receipt revisions. No received engagement means
unobserved engagement, not a proven bounce or dislike. A native context-menu
destination that never loads its reader may not emit a click; collection remains
best effort.

`exposures.search_query` uses the existing recent-search normalization and
200-rune bound. This analytics context has no age-based expiry, like its source
engagement and exposures. The separate PostgreSQL recent-search autocomplete
history retains its 180-day last-use expiry. Account
erasure removes exposures and engagement; anonymization strips query text and
origin IDs. The existing explicit exposure-forgetting methods still remove
query attribution. Clearing recent autocomplete history retains its existing
PostgreSQL-only behavior and does not erase analytics exposures.

This change collects observations. It adds no jobs, scoring model, training
pipeline or automatic interpretation of short visits.
