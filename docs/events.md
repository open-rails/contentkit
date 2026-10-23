# Catalog event log

ContentKit records catalog changes in the same PostgreSQL transaction as the
change. Each app has an independent committed order. Reading events never marks
them delivered, registers a consumer, or changes another reader's position.

An embedded or remote Go client uses the same operation:

```go
page, err := engine.Client().ListEvents(ctx, catalog.EventQuery{
    After: savedCursor,
    Limit: 100,
})
```

Remote readers call `GET /v1/events?after=<cursor>&limit=100` under the
application's ContentKit prefix. The route requires the app's restricted-content
read permission and the normal staff authentication/MFA policy. Its result has
`events`, `next_cursor`, `has_more`, and `retention_seconds`. Event IDs are decimal
strings; `version` describes the payload format. Current facts identify the
content kind, parent ID and optional version ID without joining a subject that
may already have been deleted.

The reader processes a page and saves `next_cursor` in its own storage after its
side effects succeed. Retrying the same cursor returns the same retained facts.
An empty page also supplies a continuation. Optional `types` filters must stay
the same while using a cursor; a filtered page advances over excluded events.
Omitting `after` starts at the oldest retained event.

The default retention is ninety days. Applications can set `event_retention`
(a Go duration, for example `2160h`). Existing catalog maintenance prunes a
bounded expired prefix independently of reader progress. A cursor that predates
that retained prefix, or names history replaced by a restore/reset, returns
HTTP 410 with `event_cursor_expired`; resync current state before starting again.
A cursor does not confer permission or select another app.

## Upgrade from callback delivery

Stop old application writers and event-delivery workers while running the full
explicit PostgreSQL migration. Its preflight rejects a running legacy delivery
worker; that check does not replace the operator's stopped-writer fence.

SQL32 preserves all retained purge facts in a one-time handoff table before
removing the old delivered flag. The full migration converts each app's handoff
rows into its existing durable signal-erasure jobs, then removes only the rows
successfully queued in that same transaction. It also retires pending old event
delivery and billing-notification poll jobs. `SkipQueues` leaves required work
pending, and runtime validation refuses to start that app until the full
migration completes. Other apps' handoff rows remain untouched.

New purges use an explicit same-transaction `OnPurge` callback to record required
cleanup; ordinary event readers do not control deletion or other domain work.
The log has no subscriber registry, delivery ACKs, consumer offsets, per-event
River delivery jobs or automatic polling loop.
