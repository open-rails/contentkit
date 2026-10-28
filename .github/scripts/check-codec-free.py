#!/usr/bin/env python3
"""Hosts link no media codecs: every package of the modules a host imports,
except the worker's own, builds with CGO_ENABLED=0 and depends on none of
them. Only contentkit-media-worker links libvips or runs ffmpeg."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
MODULES = [".", "server", "adapters/authkit", "adapters/openrails"]
CODECS = {"github.com/open-rails/contentkit/" + p for p in ("internal/media/image", "internal/media/video", "internal/media/worker", "cmd/media-worker")}


def go(args, cwd, env):
    return subprocess.run(["go", *args], cwd=cwd, env=env, check=True, capture_output=True, text=True).stdout


def main():
    with tempfile.TemporaryDirectory(prefix="contentkit-codec-free-") as temporary:
        # One workspace of this checkout's modules, so each sees the others' source.
        work = Path(temporary) / "go.work"
        version = next(line.split()[1] for line in (ROOT / "go.mod").read_text().splitlines() if line.startswith("go "))
        work.write_text(f"go {version}\n\nuse (\n" + "".join(f"\t{ROOT / m}\n" for m in MODULES) + ")\n")
        env = dict(os.environ, GOWORK=str(work), CGO_ENABLED="0", GOFLAGS="-mod=readonly")
        failures, checked = [], 0
        for module in MODULES:
            cwd = ROOT / module
            packages = [p for p in go(["list", "-f", "{{.ImportPath}}", "./..."], cwd, env).split() if p not in CODECS]
            for line in go(["list", "-f", "{{.ImportPath}}{{range .Deps}} {{.}}{{end}}", *packages], cwd, env).splitlines():
                package, *deps = line.split()
                linked = sorted(CODECS.intersection(deps))
                if linked:
                    failures.append(f"{package} depends on {', '.join(linked)}")
            build = subprocess.run(["go", "build", "-p", "2", *packages], cwd=cwd, env=env, capture_output=True, text=True)
            if build.returncode:
                failures.append(f"{module}: CGO_ENABLED=0 build failed\n{build.stderr}")
            checked += len(packages)
    if failures:
        sys.exit("Host packages must link no media codecs:\n" + "\n".join(failures))
    print(f"{checked} host packages build without cgo and link no media codecs.")


if __name__ == "__main__":
    main()
