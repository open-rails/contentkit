# CI execution

Ready pull requests and explicit branch requests use one CI coordinator. It verifies the tested PR-plus-base merge, checks the base is still current, and reports which complete suites run. Drafts and ordinary branch/master pushes do not run source CI. Run focused tests locally while developing; CI qualifies the reviewed candidate.

| Change | Selected suites |
|---|---|
| Shared/root Go, media implementation, schema, dependencies or unknown paths | Broad source suites |
| Isolated standalone server module | Server |
| An adapter module | That adapter and its consuming integration module |
| UI package | UI package and real-service browser tests |
| Test-only file | Its owning suite |
| Known documentation-only paths | No application suite; source scan still runs |
| CI/build files, deletions, renames or uncertain diff | Full qualification |

Image-definition changes also select nonpublishing image builds. Diff or source-read errors fail the gate. A selected job must succeed; a skipped selected job or missing selection output fails. The base is checked again before `CI Gate` succeeds. The merge rule must require that gate with current-base freshness.

Go package shards, nested modules, native media and SDK/browser tests keep their existing commands and assertions. Whenever the critical suite runs, `Hosts link no media codecs` also builds every host-importable package of the root, server and adapter modules with `CGO_ENABLED=0` and refuses any that depends on `media/image`, `media/video`, `media/worker` or `cmd/media-worker`. Reusable leaf workflows have no independent automatic PR/master triggers. `CI` and `full` manual requests select every suite; individual media/UI workflows remain available for explicit diagnosis. Test selections are documented in the run summary.

Version tags publish artifacts from a commit belonging to master. npm builds and verifies the packed exports/types/styles, including matching wildcard export conditions, without repeating source unit tests. Native image builds remain artifact qualification. A package release does not itself deploy a consuming application.
