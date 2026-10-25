#!/usr/bin/env python3
"""Exercise conservative selection through the CLI and real Git diffs."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

selector = Path(__file__).with_name("ci-scope.py").resolve()
keys = {"critical", "scan", "images"}
source = keys - {"images"}
identity = {"GIT_AUTHOR_NAME": "Paul Fidika", "GIT_AUTHOR_EMAIL": "paul@fidika.com",
            "GIT_COMMITTER_NAME": "Paul Fidika", "GIT_COMMITTER_EMAIL": "paul@fidika.com"}
env = os.environ | identity
cases = [
    ("docs", [("M", "README.md")], set()),
    ("license", [("M", "LICENSE")], set()),
    ("ui-runtime", [("M", "sdk/ui/src/components/button.tsx")], source),
    ("client", [("M", "sdk/ui/src/client/client.ts")], source),
    ("unknown-sdk-source", [("A", "sdk/ui/src/new.go")], source),
    ("unknown", [("A", "new-domain/new.go")], source),
    ("shared-dependency", [("M", "go.mod")], keys),
    ("query", [("M", "catalog/queries/content.sql")], source),
    ("generated-doc", [("M", "docs/api/routes.md")], source),
    ("sdk-doc", [("M", "sdk/ui/README.md")], set()),
    ("worker-image", [("M", "media/worker/Dockerfile")], keys),
    ("deleted-doc", [("D", "README.md")], keys),
    ("renamed-doc", [("R", "README.md")], keys),
    ("mixed", [("M", "README.md"), ("M", "sdk/ui/src/client/client.ts")], source),
    ("empty", [], keys),
]
for label, changes, expected in cases:
    with tempfile.TemporaryDirectory(prefix="contentkit-selector-") as temporary:
        root = Path(temporary)
        def git(*args):
            return subprocess.check_output(["git", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", *args], cwd=root, env=env, text=True).strip()
        git("init", "-q", "--initial-branch=main")
        for name in {"seed.txt", *(path for status, path in changes if status != "A")}:
            p = root / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("original " + name + "\n")
        git("add", ".")
        git("commit", "-q", "-m", "selector baseline")
        base = git("rev-parse", "HEAD")
        for status, name in changes:
            p = root / name
            if status == "D":
                p.unlink()
            elif status == "R":
                p.rename(root / "RENAMED.md")
            else:
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text("changed " + name + "\n")
        if changes:
            git("add", "-A")
            git("commit", "-q", "-m", "selector input")
        head = git("rev-parse", "HEAD")
        output = root / "github-output"
        result = json.loads(subprocess.check_output(["python3", str(selector), "--base", base, "--head", head, "--github-output", str(output)], cwd=root, text=True))
        assert set(result["suites"]) == keys, (label, result)
        actual = {key for key, selected in result["suites"].items() if selected}
        assert actual == expected, (label, actual, expected)
        emitted = dict(line.split("=", 1) for line in output.read_text().splitlines())
        assert emitted == ({key: str(value).lower() for key, value in result["suites"].items()} | {"suite": result["critical_suite"]})
        expected_suite = "none" if "critical" not in expected else "sdk" if label in {"ui-runtime", "client", "mixed"} else "all"
        assert result["critical_suite"] == expected_suite, (label, result)
        assert result["reasons"] and set(result["suite_explanations"]) == keys
full = json.loads(subprocess.check_output(["python3", str(selector), "--full"], text=True))
assert all(full["suites"].values())
invalid = subprocess.run(["python3", str(selector), "--base", "missing", "--head", "missing"], capture_output=True)
assert invalid.returncode != 0
print(f"Conservative selection passed {len(cases)} real-Git cases, manual full selection and invalid-reference refusal.")
