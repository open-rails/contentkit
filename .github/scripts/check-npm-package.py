"""Verify that a built npm tarball contains its declared public exports."""
import fnmatch
import json
import sys
import tarfile

path, expected = sys.argv[1:]
with tarfile.open(path, "r:gz") as archive:
    members = {member.name.removeprefix("package/") for member in archive.getmembers() if member.isfile()}
    package = json.load(archive.extractfile("package/package.json"))
if package["name"] != "@openrails/contentkit-ui" or package["version"] != expected:
    raise SystemExit("packed package identity does not match the release")

def targets(value):
    if isinstance(value, str):
        yield value.removeprefix("./")
    elif isinstance(value, dict):
        for child in value.values():
            yield from targets(child)
    elif isinstance(value, list):
        for child in value:
            yield from targets(child)

for target in [package["types"], package["style"], *targets(package["exports"])]:
    target = target.removeprefix("./")
    if not any(fnmatch.fnmatchcase(member, target) for member in members):
        raise SystemExit(f"packed export is missing: {target}")
print(f"Verified {package['name']}@{expected}: declared exports/types/styles are in the tarball")
