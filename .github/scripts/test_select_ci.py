import importlib.util
import os
import subprocess
import tempfile
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("select_ci", Path(__file__).with_name("select-ci.py"))
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)


class SelectionTests(unittest.TestCase):
    def selection(self, path, status="M"):
        return ci.select([(status, (path,))])

    def test_unknown_shared_and_dependencies_are_broad(self):
        for path in ("new.xyz", "client.go", "go.mod", "go.sum", "media/manifests.go", "migrations/0039.sql", "api/openapi.json", "docs/api/routes.md", "server/new-tool.xyz"):
            with self.subTest(path=path):
                self.assertEqual(self.selection(path)[0], ci.ALL)

    def test_isolated_modules_include_consumers(self):
        cases = {"server/main.go": {"server"}, "adapters/authkit/binding.go": {"authkit_adapter", "e2e"}, "adapters/openrails/access.go": {"openrails_adapter", "e2e"}, "e2e/account_test.go": {"e2e"}, "sdk/ui/src/Card.tsx": {"ui_unit", "ui_browser"}}
        for path, expected in cases.items():
            with self.subTest(path=path):
                self.assertEqual(self.selection(path)[0], expected)

    def test_test_only_change_selects_owning_suite(self):
        for path, suite in (("media/video/codec_test.go", "media_video"), ("media/worker/ready_test.go", "media_image"), ("catalog/read_test.go", "core")):
            self.assertEqual(self.selection(path)[0], {suite})

    def test_docs_only_are_explicit(self):
        self.assertFalse(self.selection("README.md")[0])
        self.assertFalse(self.selection("docs/guide.md")[0])
        self.assertEqual(self.selection("internal/guide.md")[0], ci.ALL)

    def test_workflow_and_images_validate_execution(self):
        self.assertEqual(self.selection(".github/workflows/go.yaml")[:3], (ci.ALL, True, True))
        self.assertTrue(self.selection("server/cmd/contentkit/Dockerfile")[1])

    def test_deletions_and_renames_never_skip(self):
        self.assertEqual(self.selection("README.md", "D")[0], ci.ALL)
        self.assertEqual(ci.select([("R100", ("old.go", "sdk/ui/new.ts"))])[0], ci.ALL)

    def test_manual_empty_and_large_diff_are_full(self):
        self.assertEqual(ci.select([], True)[:3], (ci.ALL, True, True))
        self.assertEqual(ci.select([])[0], ci.ALL)
        self.assertEqual(ci.select([("M", ("README.md",))] * 3001)[0], ci.ALL)

    def test_real_git_rename_and_missing_base_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", "-C", directory, *args], stderr=subprocess.DEVNULL).decode().strip()
            git("init", "-q")
            git("config", "user.name", "Paul Fidika")
            git("config", "user.email", "paul@fidika.com")
            (root / "old.txt").write_text("unchanged bytes\n")
            git("add", ".")
            git("-c", "commit.gpgsign=false", "commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            (root / "server").mkdir()
            (root / "old.txt").rename(root / "server/new.go")
            git("add", "-A")
            git("-c", "commit.gpgsign=false", "commit", "-qm", "rename")
            head = git("rev-parse", "HEAD")
            output, summary = root / "output", root / "summary"
            env = {**os.environ, "GITHUB_EVENT_NAME": "pull_request", "PR_BASE_SHA": base, "GITHUB_SHA": head, "GITHUB_OUTPUT": str(output), "GITHUB_STEP_SUMMARY": str(summary)}
            command = ["python3", str(Path(ci.__file__).resolve())]
            result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("core=true", output.read_text())
            self.assertIn("rename", summary.read_text())
            output.unlink()
            env["PR_BASE_SHA"] = "f" * 40
            failed = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True)
            self.assertNotEqual(failed.returncode, 0)
            self.assertFalse(output.exists())

    def test_nul_paths_and_invalid_records(self):
        self.assertEqual(ci.parse_changes(b"M\0a b\n.go\0"), [("M", ("a b\n.go",))])
        for raw in (b"M\0truncated", b"R100\0one\0", b"?\0x\0", b"M\0../outside\0"):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                ci.parse_changes(raw)


if __name__ == "__main__":
    unittest.main()
