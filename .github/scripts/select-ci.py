"""Select conservative ContentKit suites from the verified PR merge diff."""
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess

SUITES = ("core", "e2e", "server", "authkit_adapter", "openrails_adapter", "media_image", "media_video", "ui_unit", "ui_browser")
ALL = set(SUITES)


def parse_changes(raw):
    if not raw:
        return []
    fields = raw.split(b"\0")
    if fields.pop() != b"":
        raise ValueError("unterminated diff")
    changes = []
    while fields:
        status = fields.pop(0).decode("ascii")
        if not re.fullmatch(r"[ACDMRTUXB][0-9]*", status):
            raise ValueError("unknown diff status")
        count = 2 if status[0] in "RC" else 1
        if len(fields) < count:
            raise ValueError("incomplete diff")
        paths = tuple(os.fsdecode(fields.pop(0)) for _ in range(count))
        if any(not path or path.startswith("/") or ".." in PurePosixPath(path).parts for path in paths):
            raise ValueError("invalid path")
        changes.append((status, paths))
    return changes


def suites_for(status, paths):
    path = paths[-1]
    if status not in ("A", "M"):
        return ALL, True, True, "deletion, rename, copy or type change"
    if path in ("README.md", "CHANGELOG.md", "LICENSE", "HOST_INTEGRATION.md") or (path.startswith("docs/") and not path.startswith("docs/api/") and path.endswith(".md")):
        return set(), False, False, "known documentation-only path"
    if path.startswith((".github/", "scripts/")) or PurePosixPath(path).name in ("Taskfile.yml", "Taskfile.yaml", "Makefile"):
        return ALL, True, True, "CI or build execution can affect every suite"
    module_source = path.endswith(".go") or PurePosixPath(path).name in ("go.mod", "go.sum") or "/testdata/" in path
    if path.startswith("server/") and module_source:
        return {"server"}, False, False, "isolated published server module"
    if path.startswith("adapters/authkit/") and module_source:
        return {"authkit_adapter", "e2e"}, False, False, "AuthKit adapter and its consuming integration module"
    if path.startswith("adapters/openrails/") and module_source:
        return {"openrails_adapter", "e2e"}, False, False, "OpenRails adapter and its consuming integration module"
    if path.startswith("e2e/") and module_source:
        return {"e2e"}, False, False, "isolated integration-test module"
    if path.startswith("sdk/ui/"):
        return {"ui_unit", "ui_browser"}, False, False, "UI package and its real-service consumers"
    if path.endswith("_test.go"):
        if path.startswith("media/video/"):
            suites = {"media_video"}
        elif path.startswith(("media/image/", "media/worker/", "media/versions/", "cmd/media-worker/")):
            suites = {"media_image"}
        else:
            suites = {"core"}
        return suites, False, False, "tests affect their owning suite; production source is unchanged"
    if path.endswith(".go") or path.startswith(("media/", "migrations/", "access/", "api/")) or path in ("go.mod", "go.sum", "go.work", "go.work.sum"):
        return ALL, False, False, "shared Go, media, schema, API or dependency change"
    if path.endswith("Dockerfile") or path == ".dockerignore":
        return ALL, True, False, "image definition and its source consumers"
    return ALL, True, True, "unknown path: full validation"


def select(changes, manual=False):
    if manual or not changes or len(changes) > 3000:
        reason = "manual full request" if manual else "empty or oversized diff"
        return ALL, True, True, [(reason, ALL)]
    selected, images, lint, reasons = set(), False, False, []
    for status, paths in changes:
        suites, image, workflow, reason = suites_for(status, paths)
        selected |= suites
        images |= image
        lint |= workflow
        reasons.append((f'{" -> ".join(paths)}: {reason}', suites))
    return selected, images, lint, reasons


def main():
    manual = os.environ.get("GITHUB_EVENT_NAME") != "pull_request"
    base, head = os.environ.get("PR_BASE_SHA", ""), os.environ["GITHUB_SHA"]
    if not manual and not all(re.fullmatch(r"[0-9a-f]{40,64}", value) for value in (base, head)):
        raise ValueError("missing verified base/merge identity")
    raw = b"" if manual else subprocess.check_output(["git", "diff", "--name-status", "-z", "--find-renames", base, head, "--"])
    selected, images, lint, reasons = select(parse_changes(raw), manual)
    output = {suite: str(suite in selected).lower() for suite in SUITES}
    output.update(images=str(images).lower(), workflow=str(lint).lower())
    output["go_shards"] = json.dumps((["0", "1", "2"] if "core" in selected else []) + (["e2e"] if "e2e" in selected else []))
    output["go"] = str(bool(selected & {"core", "e2e", "server", "authkit_adapter", "openrails_adapter"})).lower()
    output["ui"] = str(bool(selected & {"ui_unit", "ui_browser"})).lower()
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as stream:
        stream.write("".join(f"{key}={value}\n" for key, value in output.items()))
    with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a") as stream:
        stream.write("### Suite selection\n\n")
        for suite in SUITES:
            stream.write(f'- {suite}: {"run" if suite in selected else "skip (all changes belong to other known suites)"}\n')
        for reason, suites in reasons[:200]:
            stream.write(f'- {json.dumps(reason)}: {", ".join(sorted(suites)) or "no application suite"}\n')


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, UnicodeError, subprocess.CalledProcessError) as error:
        raise SystemExit(f"Suite selection failed; no tests may be assumed unnecessary: {error}") from error
