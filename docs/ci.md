# CI execution

GitHub Actions minutes are paid for; CI is a final check, not the test runner. Test locally first and push once.

- Pull requests to `master` only; no branch-push runs. Superseded runs are cancelled. Draft PRs run only the injection scan.
- Each workflow runs only when its paths change (docs and Markdown run nothing heavy):
  - `scan`: injected-code scan, every PR ([open-rails/helpers](https://github.com/open-rails/helpers/blob/master/docs/injection-scan.md), pinned by SHA).
  - `go`: vet + `go test -race` for everything except `media/image`, `media/video` and `cmd/media-worker`, against real PGroonga Postgres, ClickHouse+Keeper and MinIO.
  - `media-video`: `media/**` (not `media/image`), `cmd/media-worker`, `go.mod`; ffmpeg.
  - `media-image`: `media/**`, `go.mod`; libvips + ffmpeg.
  - `sdk-upload`: `sdk/upload`, the upload-handler media packages, `go.mod`.
  - `images`: PRs touching a Dockerfile or `.dockerignore` build linux/amd64 only; `v*` tags publish multi-arch.
- `full` (manual `workflow_dispatch`) runs every suite regardless of paths. Run it on `master` for full-tree qualification; it also warms the caches PRs restore.
- Nothing runs on pushes to `master`.
