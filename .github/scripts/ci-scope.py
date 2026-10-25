#!/usr/bin/env python3
"""Conservative ContentKit suite selection for an already-verified Git comparison."""
import argparse
import json
from pathlib import Path
import re
import subprocess

SOURCE = ("critical", "scan")
ALL = (*SOURCE, "images")


def sdk_source(path):
    return path.startswith("sdk/ui/src/") and Path(path).suffix in {".ts", ".tsx", ".js", ".jsx", ".css", ".svg", ".png", ".webp", ".json"}


def full():
    return {key: True for key in ALL}


def select(changes):
    selected = {key: False for key in ALL}
    reasons = []
    if not changes:
        return full(), ["empty or unknown comparison: full qualification"]
    for status, path in changes:
        if status not in ("A", "M"):
            return full(), [f"deleted, renamed or unusual Git status ({status}): {path}; full qualification"]
        name = Path(path).name
        if (name in ("go.mod", "go.sum", "package.json", "pnpm-lock.yaml", "package-lock.json", "yarn.lock")
                or path.startswith((".github/", "scripts/", "tools/"))
                or name.startswith("Dockerfile") or path == ".dockerignore"):
            selected.update(full())
            reasons.append("dependency, CI or image-build input: " + path)
            continue
        if path == "LICENSE" or path.endswith(".md") and ("/" not in path or path.startswith(("docs/", "sdk/ui/"))) and not path.startswith("docs/api/"):
            reasons.append("isolated documentation: behavioral and artifact inputs unchanged: " + path)
            continue
        selected.update({key: True for key in SOURCE})
        reasons.append(("SDK source: reviewed TypeScript contracts: " if sdk_source(path) else "shared or unclassified source: full critical backend/browser suite: ") + path)
    return selected, reasons


def read_changes(base, head):
    raw = subprocess.check_output(["git", "diff", "--name-status", "-z", "--find-renames", base, head, "--"])
    tokens = raw.decode().split("\0")
    if tokens[-1] != "":
        raise ValueError("incomplete Git diff")
    changes = []
    cursor = 0
    while cursor < len(tokens) - 1:
        status, path = tokens[cursor:cursor + 2]
        cursor += 2
        changes.append((status, path))
        if status.startswith(("R", "C")):
            if cursor >= len(tokens) - 1:
                raise ValueError("incomplete renamed path")
            cursor += 1
    return changes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base")
    parser.add_argument("--head")
    parser.add_argument("--full", action="store_true")
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    if args.full:
        selected, reasons = full(), ["explicit full qualification"]
    else:
        if not all(re.fullmatch(r"[0-9a-f]{40}", value or "") for value in (args.base, args.head)):
            parser.error("base and head must be full commit SHAs")
        selected, reasons = select(read_changes(args.base, args.head))
    critical_suite = "none"
    if selected["critical"]:
        changes = [] if args.full else read_changes(args.base, args.head)
        behavioral = [path for status, path in changes if select([(status, path)])[0]["critical"]]
        sdk_only = bool(behavioral) and all(sdk_source(path) for path in behavioral)
        critical_suite = "sdk" if sdk_only and not selected["images"] else "all"
    if args.github_output:
        with args.github_output.open("a") as output:
            output.write("suite=" + critical_suite + "\n")
            for key, value in selected.items():
                output.write(f"{key}={str(value).lower()}\n")
    explanations = {}
    for suite, enabled in selected.items():
        if not enabled:
            explanations[suite] = {"selected": False, "reason": "No changed input reaches this suite under the explicit isolation rules."}
        elif args.full:
            explanations[suite] = {"selected": True, "reason": "Explicit full qualification."}
        else:
            inputs = [reason for change in read_changes(args.base, args.head)
                      for flags, why in [select([change])] if flags[suite] for reason in why]
            explanations[suite] = {"selected": True, "affected_inputs": len(inputs),
                                   "examples": inputs[:8] or reasons}
    print(json.dumps({"suites": selected, "critical_suite": critical_suite, "reasons": reasons, "suite_explanations": explanations}, indent=2))


if __name__ == "__main__":
    main()
