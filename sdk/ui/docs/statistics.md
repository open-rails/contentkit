# Measured content statistics

`StatsDashboard`, `useActivity`, `useTopContents` and `client.stats` expose the
same statistics queries as the ContentKit API. Staff need the content statistics
permission. Account statistics remain available separately through `useMyStats`
and `client.stats.mine()`.

`GET /stats/activity?kind=gallery&days=7` returns one row per UTC day:

- `readers`: distinct account or browser identities with at least one second of
  measured, focused attention and actual loaded/visible content coverage.
- `users`: the signed-in identities among those people.
- `active_ms`: measured focused active milliseconds, including positive partials
  below the one-second qualification threshold.

Opening a route, preloading an image or turning a page is not an activity count.
Actual attention and coverage are measured separately. Daily people cannot be
summed into a unique-person count for the whole period, since one person may
participate on multiple days. The dashboard sums active time only.

`GET /stats/top?kind=gallery&metric=readers&period=30d` ranks by distinct qualified
people across that period. `metric=comments` ranks by published comments and still
works without the signal plane. The SDK accepts only `readers` and `comments`;
there is no route-open, raw-view or session-count metric or compatibility alias.
This staff ranking by people is separate from the public popularity policy,
which also uses bounded per-person quality and event-time audience normalization.

The activity table displays active time as `m:ss` or `h:mm:ss`. The server retains
millisecond precision; the display truncates incomplete seconds. Existing account
completion statistics continue to require measured coverage and attention, rather
than assuming that reaching the last page means every earlier page was read.
