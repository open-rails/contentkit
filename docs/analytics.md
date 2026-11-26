# Analytics

ClickHouse takes the high-volume, append-only intake; PostgreSQL holds each reader's state and history. No product page reads ClickHouse: lists, history, continue reading, card progress and hidden galleries read PostgreSQL. Only the staff report (`GET /v1/stats/active`) and the rankings ([popularity](popularity.md)) read the rollups.

## Intake

- The browser sends `POST /v1/activity`: `open` (the content, and the list it came from: the response's `request_id`, the surface and the 1-based position) and `progress` (active time since the last report, the furthest and current page or second, and the length). The SDK sends progress on leaving and every five active minutes, skipped when unchanged.
- An open's surface is kept in `interaction_history` after the log expires, as one of: none (outside), `home`, `list` or `channel`, `popular`, `similar`, `search` or `search_suggestions`, `featured`, `entity` or an entity kind's name, `recommendations` (or `for_you`), `continue` or `history`, `favorites`; any other name counts as other. The SDK's lists send these.
- The server logs the rest where it happens: reactions, favorites, hides and searches (the normalized query, the response's `request_id` and the results in order).
- A row's subject is the account id, else a UUIDv8 hash of the browser's anonymous id (`ContentKit-Anonymous-Id`). The API stamps the site (domain and language), the access (`anonymous`, `account` or the tiers the token claims), the local UTC offset (`ContentKit-UTC-Offset`, else `cf-timezone`) and the time.
- A player's progress report may not pass the probed duration by more than two seconds.
- Each process buffers rows and writes them every second as one `INSERT … VALUES` with `async_insert=1, wait_for_async_insert=1`. While ClickHouse is down, up to 50,000 rows wait; past that the oldest are dropped.

## Storage

| Store | Table | Holds |
|---|---|---|
| ClickHouse | `activity` | every row above, 90 days, by subject and day |
| ClickHouse | `content_daily` | people per gallery × site × day by read class and the day's last reaction, split by audience |
| ClickHouse | `audience_daily` | active people per site × access × day |
| ClickHouse | `erasures` | erased subjects |
| PostgreSQL | `reading_state` | per person × gallery: first day, last seen, best outcome, furthest point, last position, hidden |
| PostgreSQL | `interaction_history` | per person × UTC day: the day's actions (opens, searches, follows) packed in time order, each gallery with the surface of its first open (see the comment in `0002_interaction_history.up.sql`) |
| PostgreSQL | `anonymous_links` | an anonymous id signed in as an account |
| PostgreSQL | `anonymous_activity` | a signed-out subject's latest activity, for expiry |
| PostgreSQL | `outcome_values` | P(sequel) and q per segment and outcome |

A day of `content_daily` and `audience_daily` is rebuilt whole under a new `generation`; read each day's newest generation.

## The analytics job

One job (`contentkit_analytics`), every 15 minutes, one run at a time:

1. Folds new sign-in links: the anonymous id's `reading_state` and `interaction_history` merge into the account's, and its reactions and recent searches are re-keyed where the account has none. Later rows of that anonymous id count as the account's.
2. Recomputes every person × gallery × day with rows ingested since its watermark (ClickHouse's ingestion time, a minute's overlap): the outcome from the fraction read, merged into `reading_state` by maximum and latest, and the day's `interaction_history` rewritten.
3. Rebuilds today's rollups hourly and yesterday's once after 00:15 UTC.
4. Deletes the rows of subjects erased since its last delete, daily: a lightweight `DELETE` naming them, 500 at a time, with a day of overlap for rows still buffered at erasure.

Outcomes: a bounce is nothing past the first page, under 10 seconds and no reaction; good is 30% of the pages or video at 1.5 s a page or half real time; complete is the last page or 90% of a video at that pace; anything else is neutral. A day with only a reaction makes no reading state. Every write is a recompute or a max/latest merge, so a repeated or crashed run is harmless.

A gallery carrying a polarizing tag (for now the site's toggleable content filters) counts a reader in its audience only with positive affinity for one of its tags: a positive mean q over their last year's reads of galleries with that tag. A `not_into` dislike puts the reader outside the audience either way.

## Signed-out expiry

A signed-out subject idle longer than `anonymous_retention` (30 days by default) is expired by `contentkit_anonymous_expiry`, an hourly PostgreSQL-only job that runs with or without ClickHouse, up to 10,000 subjects a run. Its `reading_state`, `interaction_history` and recent searches are deleted; its reactions stay, and so do their counts, but lose `anonymous_subject`. Activity is `anonymous_activity.last_active_at`: the analytics job records what it folds from the log, and a signed-out search, reaction or hide records itself. Accounts and anonymous ids linked to one are never expired. ClickHouse rows expire with the log's 90 days. A visitor returning later starts fresh.

## Erasure and clearing

- `AnonymizeAccount` records the account and its folded anonymous ids in `erasures` (every read then drops their rows), deletes their reading state, history and links, and records the anonymous ids as erased so late rows never count.
- `DELETE /v1/me/history` clears the reader's reading state and history and deletes the matching activity rows; hidden galleries stay hidden.
- A purged content item's reading state goes with the PostgreSQL purge; the content sweep deletes its activity and `content_daily` rows.
