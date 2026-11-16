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
| Into `rc/**` | Light checks: workflow lint, source scan, build and vet of every module, public-surface boundaries, `Hosts link no media codecs`, and the UI package's generated contract, types, lint and packed exports. No services, no critical suite, no image builds |
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

Version tags publish artifacts from a commit belonging to master. A UI prerelease tag may instead name an unmerged commit whose CI run passed the critical suite and `CI Gate`; a light run does not qualify it. npm builds and verifies the packed exports/types/styles, including matching wildcard export conditions, without repeating source unit tests. Native image builds remain artifact qualification. A package release does not itself deploy a consuming application.
