# Branches, CI and releases

## Branches and releases

- `master` is the release mainline. It takes pull requests only from `rc/*` (a release candidate) and `release/*` (release preparation, such as module pins); the `Source branch` check refuses any other head.
- Features merge into the open release candidate, `rc/<version>`, named for the version it will become (`rc/1.2.0`). Its copy of [OPEN.md](../OPEN.md) names it and what it is for. Check that before opening or merging a pull request: work merged into a stale `rc/*` reaches no release.
- One release candidate is open at a time. It is cut from `master` with a first commit that names it in `OPEN.md`; its last commit resets `OPEN.md` to none; it merges into `master` through one pull request and its branch is then deleted.
- A release is a tag on a `master` commit. Not every `master` commit is a release. Publishing workflows refuse a tag whose commit is not on `master`.

## CI execution

Ready pull requests and explicit branch requests use one CI coordinator. It verifies the tested PR-plus-base merge, checks the base is still current, and reports which complete suites run. Drafts and ordinary branch/master pushes do not run source CI. Run focused tests locally while developing; CI qualifies the reviewed candidate.

The base and head branches choose the run:

| Pull request | Run |
|---|---|
| Into `rc/**` | Light checks: workflow lint, supply-chain guards, source scan, build and vet of every module, public-surface boundaries, `Hosts link no media codecs`, and the UI package's generated contract, types, lint and packed exports. No services, no critical suite, no image builds |
| `rc/*` into `master` | Every suite, whatever changed |
| `release/*` into `master`, or into `release/**` | Suites selected from the changed paths, below |
| Any other head into `master` | `Source branch` fails, no suite starts and `CI Gate` fails |

Selection by changed path:

| Change | Selected suites |
|---|---|
| Shared/root Go, media implementation, schema, dependencies or unknown paths | Broad source suites |
| Isolated standalone server module | Server |
| An adapter module | That adapter and its consuming integration module |
| UI package | UI package and real-service browser tests |
| Test-only file | Its owning suite |
| Known documentation-only paths | No application suite; source scan still runs |
| CI/build files, deletions, renames or uncertain diff | Full qualification |

Image-definition changes also select nonpublishing image builds. Diff or source-read errors fail the gate. A selected job must succeed; a skipped selected job or missing selection output fails. The base is checked again before `CI Gate` succeeds. The merge rules for `master`, `rc/**` and `release/**` require that gate, on `master` and `release/**` with current-base freshness; `master` also requires `Source branch`.

Go package shards, nested modules, native media and SDK/browser tests keep their existing commands and assertions. Whenever the critical suite runs, `Hosts link no media codecs` also builds every host-importable package of the root, server and adapter modules with `CGO_ENABLED=0` and refuses any that depends on `media/image`, `media/video`, `media/worker` or `cmd/media-worker`. Reusable leaf workflows have no independent automatic PR/master triggers. `CI` and `full` manual requests select every suite; individual media/UI workflows remain available for explicit diagnosis. Test selections are documented in the run summary.

Jobs that link libvips or run FFmpeg (`Build and vet every module`, `Required Security` and the critical suite) run in the media worker's build image, `golang:…-trixie` pinned by digest, through [`native-setup`](../.github/actions/native-setup/action.yml). It installs their Debian packages from `snapshot.debian.org` at the worker Dockerfile's `DEBIAN_SNAPSHOT` with the script the image build uses, [`debian-snapshot.sh`](../cmd/media-worker/debian-snapshot.sh), so CI tests the libvips and FFmpeg builds the worker ships; no job installs from the runner's Ubuntu mirrors. The critical job reaches PostgreSQL, MinIO and ClickHouse by name on the job's network.

Version tags publish artifacts from a commit belonging to master. A UI prerelease tag may instead name an unmerged commit whose CI run passed the critical suite and `CI Gate`; a light run does not qualify it. npm builds and verifies the packed exports/types/styles, including matching wildcard export conditions, without repeating source unit tests. Native image builds remain artifact qualification. A package release does not itself deploy a consuming application.

## Supply chain

- Every `uses:` names a commit SHA with its release as a comment, and every Dockerfile base and workflow job image is pinned by digest. [`check-supply-chain.sh`](../.github/scripts/check-supply-chain.sh) refuses anything else, and any tracked `go.work*` or `.env*` file, on every ready pull request.
- Verifying holds no publish credentials. Pull-request image builds ([`verify-images.yaml`](../.github/workflows/verify-images.yaml)) run with `contents: read` and no registry login. Release tags publish images through [`images.yaml`](../.github/workflows/images.yaml) under the `release` environment, and the UI package through [`release.yaml`](../.github/workflows/release.yaml) under the `npm` environment. Each environment admits release tags only and holds its registry's credential. Dispatching `images` on a release tag republishes it.
- Trivy refuses fixable HIGH and CRITICAL vulnerabilities in the image builds of a run into `master` and in each pushed digest before `images.yaml` tags it. Published images carry an SBOM and max-mode provenance.
- The media worker and CI's native jobs install Debian packages from `snapshot.debian.org` at the worker Dockerfile's `DEBIAN_SNAPSHOT`. Move it forward, with the base image digests, to take Debian's fixes in both.
- The build context is an allowlist ([`.dockerignore`](../.dockerignore)): Go sources, module files, embedded files and the worker's package script. Image builds set `GOWORK=off`.

## Dependencies

Run [`scripts/deps`](../scripts/deps) when a release candidate is cut. It lists the repository's open advisories by manifest and package and exits 1 when a fix is published. Submit the updates as one pull request into the release candidate. On the candidate's run into `master`, `Required Security` runs govulncheck over every module and blocks a reachable vulnerability that has a fix; the advisory feed also covers code that is not reached and development dependencies.

Every module states `go` (the floor for consumers) and `toolchain` (the patched release that builds and scans use). The six modules, the image Go stages and CI's native job image move in step. A dependency bump goes into every module that carries it, followed by `go mod tidy` and `go mod verify` in each. `authkit` and `openrails` follow their upstream releases, not this batch.

A JavaScript bump updates `sdk/ui/pnpm-lock.yaml` with `pnpm install --lockfile-only --config.optimistic-repeat-install=false`. A patched transitive version that its parents' ranges allow but resolution keeps old goes in `overrides` in `sdk/ui/pnpm-workspace.yaml`. `minimumReleaseAge` holds back any release younger than a day.
