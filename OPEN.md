# Open release candidate

`rc/0.139.0` carries the fixes from the 2026-11-23 security audit: bounded search, a per-request public read boundary and cache directives, the standalone server's client address behind proxies, import and erasure contracts, media work bounds, and a pinned supply chain.

Feature pull requests target the open release candidate, never `master`. A release candidate's first commit names its
`rc/<version>` branch and purpose here; its last commit before it merges into `master` resets this file. So on `master`, and
on a candidate that has been promoted, this reads "None"; on the open candidate (`git ls-remote --heads origin 'rc/*'`)
it names that branch. See [docs/ci.md](docs/ci.md#branches-and-releases).
