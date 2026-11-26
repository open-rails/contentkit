# Popularity

Popular lists rank contents and entities by how well they were read on the reader's site. Rankings are computed from ClickHouse's `content_daily` and cached in Redis (in process without it); nothing about them is stored in PostgreSQL.

## Score

A content's popularity over a window is the sum of q over its person-days on the site: a person counts once per content per UTC day, weighted by the quality of that day's read. q runs from −1 to +1 around the segment's base rate and comes from `outcome_values` (`internal/outcomevalue`): a day's last reaction, keyed with its reason when one was given (`dislike:not_into`), else its read class (bounce, neutral, good, complete). Many readers with good reads rank highest; a gallery split between favourites and "not into" scores about zero however often it is opened. Reading time never adds weight, and there is no time decay.

- **Site.** A site is a domain (`kinds.Config.DomainFor(host).Name`) and a language: the request's `language`, else its first configured `Accept-Language` tag, else the first configured language. Each site ranks its own reads.
- **Gated days.** A day a content was open only to tiers (its access history at the day's midpoint) is scaled by the site's active people over the active people holding one of those tiers that day (`audience_daily`). Days open only to buyers or a channel's members are not scaled.
- **Trending.** The week ranks per live day: the sum divided by the days the content was live in the window, today counting as the part elapsed.
- **Entities.** An entity's popularity is the sum of its contents', through the catalog's content → entity mapping (held in memory, reloaded when the catalog changes). Its polarization, 0 to 1, is how split its readers are between strong positives and strong negatives (|q| ≥ 0.5): 2·min(P, N) / (P + N + 20).
- **Audience.** A list of one entity's contents (`groups`) ranks by its audience's popularity: readers outside a content's audience (a "not into" answer for its tag) don't count there.
- **Read-through.** An artist kind's (`artist: true`) cards show its read-through rate: of the people whose read of one of its works was good or complete before the next was released, and who were active after that release, the share who read the next within 30 days. Computed from `reading_state`, cached for 12 hours.

## Windows

`window` takes:

- a featured window, including today so far: `week` (the default), `30d`, `90d`, `year` (the calendar year), `all`;
- a calendar month `month:YYYY-MM` or year `year:YYYY`;
- UTC days `YYYY-MM-DD..YYYY-MM-DD`, both included.

Featured windows are precomputed after each hourly rebuild of `content_daily`, for every site, list and window, in one ClickHouse query per site (`rankings.Engine.Precompute`, the `contentkit_rankings` job). Pages read only the cache; while a precompute fails, the previous ranking keeps being served, marked `stale` after three hours. A window that was never precomputed lists in its fallback order with `ready: false`.

A closed range, one whose last day has had its final (post-midnight) rebuild, never changes for one `outcome_values` version: it is computed on first request, under a lock across replicas, and kept without expiry. A refit makes a new version, so each closed range recomputes once. A month's and a year's range is filled right after its last day's final rebuild. Any other range (one including today) is computed per request, uncached, two at a time per process.

## Lists

A popular list shows everything its filters keep: the ranked entries with a positive or zero score in order, then those without activity in the window (contents newest first, entities by content count), then the negative ones. A page takes the next chunk of the cached ranking and reads it from PostgreSQL with the viewer's filters and access, so it drops only what they block: the site's hidden entities, the account's `content_filter_prefs` overrides or a signed-out visitor's browser choices. Its cursor carries the ranking's version, so a refresh never shifts a reader's pages. A selective list (entities, a channel, creators, ids, a keyword match) orders its whole selection by the ranking instead.

- `GET /contents?kind=…&sort=popular&window=…` and `GET /search?…&sort=popular`.
- `GET /entities?kind=…` defaults to `sort=popular`; `window` applies. Cards carry `popularity` (score, polarization, read-through).
- Responses carry `popularity`: the window, its days, when it was ranked, `ready` and `stale`.

## Similar

`GET /contents/{id}/similar` lists contents sharing the anchor's most specific entities, each weighted by ln(1 + N / its content count) so rarer ones count more, a shared artist adding 2. Computed in PostgreSQL, cached per content for six hours, and read through the viewer's filters.

## Exploration

`rankings.Engine.Exploration` counts each content's readers from `reading_state`, today's included: the people who opened it and those whose best read was good or complete.
