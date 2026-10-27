# Catalog event log

ContentKit records catalog changes in the same PostgreSQL transaction as the
change. Each app has an independent committed order. Reading events never marks
them delivered, registers a consumer, or changes another reader's position.

An embedded or remote Go client uses the same operation:

```go
page, err := engine.Client().EventReader().ListEvents(ctx, eventlog.Query{
    After: savedCursor,
    Limit: 100,
})
```

`eventlog` is `github.com/open-rails/helpers/eventlog`. `Client.EventReader()`
uses the existing embedded or HTTP transport and returns the shared envelope;
`ListEvents` remains available with its catalog-specific records and the same
cursors. Engine clients retain host authority; remote clients retain the
caller's credentials. Shared records include source, app scope, subject, payload,
and separate occurrence and recording timestamps. Imported legacy records keep
the mutation insertion time in both fields; new records keep the occurrence
time and the shared log's commit-time recording timestamp separately.

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
Omitting `after` starts at the oldest retained event. Filters are sorted and
deduplicated; unknown future types are valid. The limit defaults to 100 and
cannot exceed 500; at most 100 type filters are accepted. HTTP clients can send
repeated `type` parameters for exact values (including commas); these take
precedence over the legacy comma-separated `types` parameter.

The default retention is ninety days. Applications can set `event_retention`
(a Go duration, for example `2160h`). Reads exclude expired records even before
maintenance runs, including expired rows after a younger row. Existing catalog maintenance prunes a
bounded expired prefix independently of reader progress. A cursor that predates
that retained prefix, or names history replaced by a restore/reset, returns
HTTP 410 with `event_cursor_expired`; resync current state before starting again.
A cursor does not confer permission or select another app.

## Purge recording

New purges use an explicit same-transaction `OnPurge` callback to record required
cleanup; ordinary event readers do not control deletion or other domain work.
The log has no subscriber registry, delivery ACKs, consumer offsets, per-event
River delivery jobs or automatic polling loop.

## Shared PostgreSQL storage

Retained facts live in the shared Helpers event log in the catalog schema.

New facts enter the shared pending queue in the domain transaction. At commit,
the shared flush locks the touched scopes in sorted order and assigns positions.
Numeric event IDs can therefore commit out of order; only cursors and stream
positions define ordering. Discovery invalidates its summary cache with the
committed position, independently of ID allocation and pruning. Reading an empty
app still makes no writes and keeps previously issued empty cursors valid.

The schema-global epoch remains authoritative for public catalog cursors. Shared
stream epochs are seeded identity metadata; an older transaction may create a
stream after a global rotation, so its stored epoch cannot override that reset.

Audience release schedules use `version.audience_released` facts. The payload
includes `content_id`, `version_id` and the released audience (`release.at`,
`release.level`, and its optional `tiers` or `group_id`). `occurred_at` is when
the audience actually became eligible: no earlier than its configured timestamp,
publication, policy introduction, or restoration; `recorded_at` is when the fact
entered the log. The common release worker applies each scheduled audience and
records its facts through the catalog completion transaction. Content edits
first retain any already-effective release facts,
so unpublishing or rescheduling cannot erase an already-effective release.
