import hashlib
import importlib.util
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).with_name("collect-run-data.py")
SPEC = importlib.util.spec_from_file_location("collect_run_data", SCRIPT)
collect_run_data = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(collect_run_data)

LOG_FILES = [
    "logs/dustkeeper.jsonl",
    "logs/dustkeeper.jsonl.1",
    "logs/dustkeeper.jsonl.2",
    "logs/dustkeeper.jsonl.3",
]
ALL_SOURCES = LOG_FILES + ["reports/latest.json"]


def make_state(root, sources=ALL_SOURCES):
    state = root / "state"
    (state / "logs").mkdir(parents=True, exist_ok=True)
    (state / "reports").mkdir(parents=True, exist_ok=True)
    for rel in ALL_SOURCES:
        if rel in sources:
            (state / rel).write_bytes(("evidence " + rel + "\n").encode())
    # Things that must never be collected: a recovery backup needs these, and
    # their presence here proves the whitelist excludes them.
    (state / "state.db").write_bytes(b"sqlite-secret")
    (root / "config.json").write_bytes(b"api-key")
    return state


def collect(root, state, output):
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--state-dir", str(state), "--output", str(output)],
        text=True, capture_output=True,
    )


def read_manifest(zip_path):
    with zipfile.ZipFile(zip_path) as zf:
        return json.loads(zf.read(collect_run_data.MANIFEST_NAME)), zf.namelist()


class CollectRunDataTest(unittest.TestCase):
    def test_complete_collection_is_private_and_only_contains_the_whitelist(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root)
            out = root / "run.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("complete: yes", result.stdout)
            self.assertIn("not a consistent recovery backup", result.stdout)
            self.assertEqual(stat.S_IMODE(out.stat().st_mode), 0o600)

            manifest, names = read_manifest(out)
            self.assertEqual(manifest["schema_version"], 1)
            self.assertTrue(manifest["complete"])
            self.assertEqual(manifest["source_total_cap_bytes"], 4 * ((2 << 20) + (64 << 10)) + (4 << 20))
            self.assertEqual(sorted(names), sorted(ALL_SOURCES + [collect_run_data.MANIFEST_NAME]))
            self.assertNotIn("state.db", names)
            self.assertNotIn("config.json", names)
            self.assertIn("state.db", " ".join(manifest["not_collected"]))
            for rel in ALL_SOURCES:
                record = next(e for e in manifest["files"] if e["name"] == rel)
                self.assertEqual(record["status"], "ok")
                with zipfile.ZipFile(out) as zf:
                    body = zf.read(rel)
                self.assertEqual(record["sha256"], hashlib.sha256(body).hexdigest())
                self.assertEqual(record["bytes_included"], len(body))

    def test_missing_rotated_logs_are_marked_not_silently_dropped(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=[LOG_FILES[0], "reports/latest.json"])
            out = root / "partial.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest, names = read_manifest(out)
            self.assertEqual(sorted(names), sorted([LOG_FILES[0], "reports/latest.json", collect_run_data.MANIFEST_NAME]))
            self.assertFalse(manifest["complete"])
            self.assertEqual(manifest["counts"]["missing"], 3)
            self.assertTrue(any("optional rotated log" in g for g in manifest["gaps"]))
            self.assertEqual(len(manifest["files"]), 5)

    def test_missing_required_sources_are_gaps(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=[])
            out = root / "empty.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest, names = read_manifest(out)
            self.assertEqual(names, [collect_run_data.MANIFEST_NAME])
            self.assertFalse(manifest["complete"])
            self.assertTrue(any(g.startswith("logs/dustkeeper.jsonl: missing") for g in manifest["gaps"]))
            self.assertTrue(any(g.startswith("reports/latest.json: missing") for g in manifest["gaps"]))

    def test_oversize_source_is_not_read_but_is_recorded(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=[])
            (state / "logs/dustkeeper.jsonl").write_bytes(b"x" * ((2 << 20) + (64 << 10) + 1))
            (state / "reports/latest.json").write_bytes(b"{}")
            out = root / "oversize.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest, names = read_manifest(out)
            big = next(e for e in manifest["files"] if e["name"] == LOG_FILES[0])
            self.assertEqual(big["status"], "oversize")
            self.assertEqual(big["bytes_included"], 0)
            self.assertIsNone(big["sha256"])
            self.assertNotIn(LOG_FILES[0], names)
            self.assertFalse(manifest["complete"])

    def test_symlink_on_the_whitelisted_path_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=["reports/latest.json"])
            outside = root / "outside.jsonl"
            outside.write_bytes(b"not from this instance")
            (state / "logs/dustkeeper.jsonl").symlink_to(outside)
            out = root / "symlink.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest, names = read_manifest(out)
            linked = next(e for e in manifest["files"] if e["name"] == LOG_FILES[0])
            self.assertEqual(linked["status"], "refused")
            self.assertIn("symlink", linked["reason"])
            self.assertNotIn(LOG_FILES[0], names)
            self.assertFalse(manifest["complete"])

    def test_a_directory_where_a_regular_file_is_expected_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=["reports/latest.json"])
            (state / "logs/dustkeeper.jsonl").mkdir()
            out = root / "dir.zip"
            result = collect(root, state, out)
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest, _ = read_manifest(out)
            odd = next(e for e in manifest["files"] if e["name"] == LOG_FILES[0])
            self.assertEqual(odd["status"], "refused")
            self.assertIn("not a regular file", odd["reason"])

    def test_existing_output_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root)
            out = root / "already.zip"
            out.write_bytes(b"previous evidence")
            result = collect(root, state, out)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("never overwrite", result.stderr)
            self.assertEqual(out.read_bytes(), b"previous evidence")

    def test_relative_paths_are_refused_before_anything_is_written(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root)
            out = root / "relative.zip"
            bad = subprocess.run(
                [sys.executable, str(SCRIPT), "--state-dir", "state", "--output", str(out)],
                text=True, capture_output=True,
            )
            self.assertNotEqual(bad.returncode, 0)
            self.assertIn("absolute", bad.stderr)
            self.assertFalse(out.exists())

    def test_state_dir_must_exist(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            out = root / "missing-state.zip"
            result = collect(root, root / "no-such-state", out)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not a directory", result.stderr)
            self.assertFalse(out.exists())

    def test_a_change_during_the_read_is_observable_in_the_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = make_state(root, sources=[])
            target = state / "logs/dustkeeper.jsonl"
            target.write_bytes(b"first\n")
            real_read = os.read
            appended = [False]

            def append_then_read(fd, size):
                if not appended[0]:
                    with open(target, "ab") as growing:
                        growing.write(b"second\n")
                    appended[0] = True
                return real_read(fd, size)

            with mock.patch("os.read", side_effect=append_then_read):
                info, data = collect_run_data.read_bounded(target, collect_run_data.MAX_LOG_BYTES)
            self.assertIn(b"first", data)
            self.assertEqual(info["status"], "changed")
            self.assertTrue(info["changed_during_read"])
            self.assertNotEqual(info["before"], info["after"])


if __name__ == "__main__":
    unittest.main()
