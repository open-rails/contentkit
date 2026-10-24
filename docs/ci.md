# CI execution

## Suite selection and releases

CI starts only for a ready pull request or an explicit branch request. The coordinator verifies the tested PR merge against its base/head, selects coherent suites and reports each decision. `CI Gate` aggregates all selected results; a draft's skipped job has a different name.

Known isolated changes select their module and direct consumers: server; either adapter plus the real integration module; UI package plus real browser tests; test-only changes select the owning suite. Shared/root Go, media implementation, schema, dependencies and unknown changes select the broad source suites. Deletions, renames, empty/oversized diffs and CI changes are conservative full requests. Diff errors fail the gate. Known documentation can skip source tests. Image-definition changes add build-only image checks.

Source tests run before merge and do not repeat automatically on master. Manual `CI` or `full` runs use the same coordinator with every suite selected. Existing media/SDK leaf workflows remain callable for explicit diagnosis, but have no independent automatic PR/master triggers.

A release tag publishes artifacts from the tested source. npm builds and checks the packed exports/types/styles; it does not rerun the unchanged source unit suite. Image publication and its native builds remain artifact qualification. A release does not claim a downstream application's deployment.

GitHub Actions minutes are paid for; CI is a final check, not the test runner. Test locally first and push once.

- Pull requests to `master` only; no branch-push runs. Superseded runs are cancelled. Draft PRs run only the injection scan.
- Each workflow runs only when its paths change (docs and Markdown run nothing heavy):
  - `scan`: injected-code scan, every PR ([open-rails/helpers](https://github.com/open-rails/helpers/blob/master/docs/injection-scan.md), pinned by SHA).
  - `go`: vet + `go test -race` for everything except `media/image`, `media/video`, `media/worker` and `cmd/media-worker`, against real PGroonga Postgres, ClickHouse+Keeper and MinIO. The `e2e` module (every route over a real AuthKit) runs there too; the `adapters/openrails` module in its own job against Postgres 18 and a real embedded OpenRails.
  - `media-video`: `media/**` (not `media/image`), `go.mod`; ffmpeg.
  - `media-image`: `media/**`, `cmd/media-worker`, `go.mod`; libvips + ffmpeg (`media/image`, `media/worker`, `cmd/media-worker`).
  - `sdk-ui`: `sdk/ui`, the shared `contenturl/testdata` vectors, the upload-handler media packages, `go.mod`.
  - `images`: PRs touching a Dockerfile or `.dockerignore` build linux/amd64 only; `v*` tags publish multi-arch.
- `release` (a published `v*` release) publishes `@openrails/contentkit-ui` at that version to npm (trusted publishing, with provenance).
- `full` (manual `workflow_dispatch`) runs every suite regardless of paths. Run it on `master` for full-tree qualification; it also warms the caches PRs restore.
- Nothing runs on pushes to `master`.
