#!/usr/bin/env python3
"""Run actual critical cases and reject incomplete or vacuous qualification.

--focus is for local development only. The default is the release-candidate
contract: every reviewed scenario and operation binding must execute and pass.
"""
import argparse
import csv
import json
import os
from pathlib import Path
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent


def check_inventory(ledger):
    with (HERE / "http_matrix.tsv").open() as source:
        rows = list(csv.DictReader(source, delimiter="\t"))
    planned = {(row["method"].lower(), row["path"]) for row in rows}
    api = json.loads((HERE.parents[1] / "api" / "openapi.json").read_text())
    declared = {(method, path) for path, operations in api["paths"].items() for method in operations}
    if planned != declared:
        raise ValueError(f"HTTP inventory drift: unplanned={declared - planned}, retired={planned - declared}")
    expected = {row["id"]: (row["method"], row["path"]) for row in rows}
    actual = {row["id"]: (row["method"], row["path"]) for row in ledger.get("operation_contracts", [])}
    if actual != expected or len(actual) != len(ledger.get("operation_contracts", [])):
        raise ValueError("HTTP ledger omits, duplicates or changes a selected operation")
    with (HERE / "binding_matrix.tsv").open() as source:
        bindings = list(csv.DictReader(source, delimiter="\t"))
    expected_bindings = {row["id"]: row["name"] for row in bindings}
    if len(expected_bindings) != len(bindings):
        raise ValueError("binding inventory duplicates a selected contract")
    actual_bindings = {row["id"]: row["name"] for row in ledger.get("binding_contracts", [])}
    if actual_bindings != expected_bindings or len(actual_bindings) != len(ledger.get("binding_contracts", [])):
        raise ValueError("binding ledger omits, duplicates or changes a selected contract")


def qualified_cases(ledger):
    check_inventory(ledger)
    pending = [row["id"] for row in ledger["required"] if row["status"] != "verified"]
    if pending:
        raise ValueError("critical behavior review is incomplete: " + ", ".join(pending))
    expected = set()
    for row in ledger["required"]:
        cases = row.get("cases", [])
        if not cases or not row.get("evidence"):
            raise ValueError(f"{row['id']} lacks reviewed executable cases/evidence")
        expected.update(cases)
    operations = ledger.get("operation_contracts", [])
    if not operations:
        raise ValueError("selected public operation contracts have not been reviewed")
    for row in operations:
        if row.get("status") != "verified" or not row.get("cases"):
            raise ValueError(f"unqualified public operation contract: {row.get('id')}")
        expected.update(row["cases"])
    bindings = ledger.get("binding_contracts", [])
    if not bindings:
        raise ValueError("selected public Go and TypeScript bindings have not been reviewed")
    for row in bindings:
        if row.get("status") != "verified" or not row.get("cases") or not row.get("evidence"):
            raise ValueError(f"unqualified public binding: {row.get('id')}")
        expected.update(row["cases"])
    return expected


def verify(events, expected):
    started, passed, packages = set(), set(), set()
    for event in events:
        action, name = event.get("Action"), event.get("Test")
        if action == "skip":
            raise ValueError(f"selected execution skipped: {name or event.get('Package')}")
        if action == "fail":
            raise ValueError(f"selected execution failed: {name or event.get('Package')}")
        if action == "run" and name:
            started.add(name)
        if action == "pass":
            if name:
                passed.add(name)
            else:
                packages.add(event.get("Package"))
    if not started or not packages:
        raise ValueError("zero executed tests or no completed test package")
    unfinished = started - passed
    if unfinished:
        raise ValueError("tests did not finish successfully: " + ", ".join(sorted(unfinished)))
    missing = expected - passed
    if missing:
        raise ValueError("selected cases did not execute: " + ", ".join(sorted(missing)))
    return len(passed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--focus", help="local top-level Go test expression; never qualification")
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--output", type=Path, default=HERE / "results" / "go.jsonl")
    parser.add_argument("--verify", type=Path, help="verify saved go -json output under the same selection contract")
    args = parser.parse_args()
    if args.focus and (os.getenv("CI") or "/" in args.focus):
        raise ValueError("focused execution is local and must select whole top-level tests")
    ledger = json.loads((HERE / "coverage.json").read_text())
    expected = set() if args.focus else qualified_cases(ledger)
    if args.verify:
        events = [json.loads(line) for line in args.verify.read_text().splitlines() if line.strip()]
    else:
        # Pure public-value selections need no database. Every integration
        # fixture independently refuses an empty DSN before opening a pool.
        required = [] if args.focus else ["CONTENTKIT_CRITICAL_PG"]
        if not args.focus or re.search("Media|Upload|Multipart|Gateway|Browser", args.focus):
            required += ["CONTENTKIT_CRITICAL_S3_ENDPOINT", "CONTENTKIT_CRITICAL_S3_USER", "CONTENTKIT_CRITICAL_S3_PASSWORD"]
        if not args.focus:
            required += ["CONTENTKIT_CRITICAL_CH_ADDR", "CONTENTKIT_CRITICAL_CH_USER", "CONTENTKIT_CRITICAL_CH_PASSWORD"]
        missing = [name for name in required if not os.getenv(name)]
        if missing:
            raise ValueError("missing real fixture settings: " + ", ".join(missing))
        env = dict(os.environ, GOWORK="off", GOMAXPROCS="2")
        command = ["go", "test", "-json", "-count=1", "-p", "1", "-timeout=15m"]
        if args.race:
            command.append("-race")
        if args.focus:
            command += ["-run", args.focus]
        command.append("./...")
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with args.output.open("w") as log:
            result = subprocess.run(command, cwd=HERE, env=env, stdout=log, stderr=subprocess.STDOUT)
        events = []
        for line in args.output.read_text().splitlines():
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                print(line, file=sys.stderr)
        if result.returncode:
            for event in events:
                if event.get("Output"):
                    print(event["Output"], end="", file=sys.stderr)
            raise ValueError(f"Go critical execution exited {result.returncode}; log: {args.output}")
    count = verify(events, expected)
    label = "LOCAL PARTIAL EXECUTION; NOT QUALIFICATION" if args.focus else "CRITICAL CONTRACT QUALIFIED"
    print(f"{label}: {count} named tests completed; {args.verify or args.output}")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError) as error:
        sys.exit(f"critical suite refused: {error}")
