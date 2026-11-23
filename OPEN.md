# Open release candidate

`rc/0.138.23` lets admins hide the whole site or chosen content from visitors by country, US state or address range, per domain, with a history of changes.

Feature pull requests target the open release candidate, never `master`. A release candidate's first commit names its
`rc/<version>` branch and purpose here; its last commit before it merges into `master` resets this file. So on `master`, and
on a candidate that has been promoted, this reads "None"; on the open candidate (`git ls-remote --heads origin 'rc/*'`)
it names that branch. See [docs/ci.md](docs/ci.md#branches-and-releases).
