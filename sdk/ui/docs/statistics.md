# Content statistics

`StatsDashboard`, `useActive` and `client.stats.active()` read `GET /stats/active?days=7`, which needs the content statistics permission. Each UTC day has:

- `people`: distinct people active on the site that day, all sites together. A person is an account, or a browser's anonymous id until it signs in.
- `by_access`: the same people by their access that day: `anonymous`, `account`, or the tiers an account's token claimed.

The analytics job rebuilds today's counts hourly and yesterday's once more after midnight UTC. People are distinct within a day only, so days can't be summed into a count of unique people over the period.

Account statistics are separate: `useMyStats` and `client.stats.mine()` count the contents a signed-in reader finished, from their reading state.
