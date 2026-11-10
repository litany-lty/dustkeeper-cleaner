import importlib.util
import io
import json
import os
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from pathlib import Path


SCRIPT = Path(__file__).with_name("run-scheduled-log.py")


def load_module():
    spec = importlib.util.spec_from_file_location("run_scheduled_log", str(SCRIPT))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ScheduledLogTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        for name in ("config", "state", "bin", "readable-reports"):
            (self.root / name).mkdir()
        (self.root / "bin/dustkeeper").write_text("binary", encoding="utf-8")
        (self.root / "bin/dustkeeper").chmod(0o700)
        (self.root / "run-readable-log.py").write_text("wrapper", encoding="utf-8")
        (self.root / "config/review-credentials.json").write_text(
            json.dumps({"typesafe_api_key": "jev-secret", "minimax_api_key": "sk-cp-subscription"}), encoding="utf-8")
        (self.root / "config/config.json").write_text(json.dumps({"review": {
            "credentials_file": str(self.root / "config/review-credentials.json"),
            "jev_model": "jev-fixed", "minimax_model": "minimax-fixed",
            "max_candidates": 1, "batch_max_candidates": 1, "batch_max_attempts": 2,
        }}), encoding="utf-8")
        self.write_ledger()
        self.mod = load_module()

    def tearDown(self):
        self.tmp.cleanup()

    def write_ledger(self, **updates):
        body = {
            "schema": 1, "limit_millicny": 70000, "used_millicny": 0,
            "models": {"jev": "jev-fixed", "minimax": "minimax-fixed"},
            "pending": None, "rounds": 0, "http_attempts": 0, "history": [],
        }
        body.update(updates)
        (self.root / "automation-budget.json").write_text(json.dumps(body), encoding="utf-8")

    def receipt(self, run_id, attempts=2, exit_code=0, schema=3):
        now = datetime.now(timezone.utc).astimezone().replace(microsecond=0).isoformat()
        return {
            "schema_version": schema, "run_id": run_id, "started_at": now, "ended_at": now,
            "exit": exit_code, "summary": {"review": {
                "http_attempts": attempts, "review_failed": 0, "unknown_held": 0,
                "cost_unknown": 0, "facts_unknown": 0, "reviewed": 1 if attempts else 0,
            }},
        }

    def runner(self, receipt=None, returncode=0):
        calls = []

        def run(cmd, env, timeout):
            calls.append((list(cmd), dict(env), timeout))
            if cmd[0] == str(self.root / "bin/dustkeeper") and cmd[1] == "pause":
                (self.root / "state/pause.request").write_text("paused", encoding="utf-8")
                return subprocess.CompletedProcess(cmd, 6, "", "")
            if receipt is not None:
                target = self.root / "readable-reports/latest-readable.json"
                target.write_text(json.dumps(receipt), encoding="utf-8")
                os.utime(str(target), None)
            return subprocess.CompletedProcess(cmd, returncode, "report\n", "")

        return run, calls

    def test_normal_round_records_bounded_receipt(self):
        old = self.root / "readable-reports/latest-readable.json"
        old_receipt = self.receipt("old")
        old_receipt["ended_at"] = "2026-11-10T16:00:01+08:00"
        old.write_text(json.dumps(old_receipt), encoding="utf-8")
        runner, calls = self.runner(self.receipt("run-new"))
        self.assertEqual(self.mod.run_once(self.root, runner=runner), 0)
        ledger = json.loads((self.root / "automation-budget.json").read_text(encoding="utf-8"))
        self.assertEqual((ledger["rounds"], ledger["http_attempts"], ledger["used_millicny"]), (1, 2, 10))
        self.assertIsNone(ledger["pending"])
        self.assertEqual(ledger["history"][-1]["run_id"], "run-new")
        wrapper = calls[0]
        self.assertEqual(len(calls), 1)
        self.assertEqual(wrapper[0][1], str(self.root / "run-readable-log.py"))
        self.assertIn(str(self.root / "bin/dustkeeper"), wrapper[0])
        self.assertEqual(wrapper[2], 960)
        self.assertEqual(wrapper[1]["DUSTKEEPER_CONFIG_DIR"], str(self.root / "config"))
        self.assertEqual(wrapper[1]["DUSTKEEPER_STATE_DIR"], str(self.root / "state"))

    def test_over_budget_and_previous_pending_do_not_start(self):
        self.write_ledger(used_millicny=69995)
        runner, calls = self.runner(self.receipt("unused"))
        output = io.StringIO()
        with redirect_stdout(output):
            self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertIn("REFUSED", output.getvalue())
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][0][1], "pause")
        self.assertFalse(any(str(self.root / "run-readable-log.py") in call[0] for call in calls))

        (self.root / "state/pause.request").unlink()
        (self.root / "automation-budget.json").write_text("not-json", encoding="utf-8")
        runner, calls = self.runner()
        with redirect_stdout(io.StringIO()):
            self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][0][1], "pause")
        self.assertEqual((self.root / "automation-budget.json").read_text(encoding="utf-8"), "not-json")
        self.assertFalse(any(str(self.root / "run-readable-log.py") in call[0] for call in calls))

        (self.root / "state/pause.request").unlink()
        self.write_ledger(used_millicny=10, pending={"reserved_millicny": 10, "started_ns": 1})
        runner, calls = self.runner()
        self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertEqual(calls[0][0][1], "pause")
        ledger = json.loads((self.root / "automation-budget.json").read_text(encoding="utf-8"))
        self.assertEqual(ledger["used_millicny"], 10)
        self.assertIsNotNone(ledger["pending"])
        self.assertTrue(ledger["pending"]["pause_confirmed"])

        (self.root / "state/pause.request").unlink()
        self.write_ledger()
        bad_time = self.receipt("run-bad-time")
        bad_time["started_at"] = "not-an-iso-time"
        runner, _ = self.runner(bad_time)
        with redirect_stdout(io.StringIO()):
            self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertTrue((self.root / "state/pause.request").is_file())

    def test_bad_receipt_pauses_and_keeps_reservation(self):
        bad = self.receipt("run-bad")
        del bad["summary"]["review"]["facts_unknown"]
        runner, calls = self.runner(bad)
        self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertEqual(calls[-1][0][1], "pause")
        ledger = json.loads((self.root / "automation-budget.json").read_text(encoding="utf-8"))
        self.assertEqual(ledger["used_millicny"], 10)
        self.assertIsNotNone(ledger["pending"])
        self.assertTrue(ledger["pending"]["pause_confirmed"])

    def test_zero_http_round_releases_reservation(self):
        receipt = self.receipt("run-zero", attempts=0, exit_code=9)
        receipt["code"] = "BUDGET_EXHAUSTED"
        receipt["summary"]["review"]["not_started"] = 1
        receipt["summary"]["scopes"] = [{"failure_code": "BUDGET_EXHAUSTED"}]
        runner, _ = self.runner(receipt, returncode=9)
        self.assertEqual(self.mod.run_once(self.root, runner=runner), 0)
        ledger = json.loads((self.root / "automation-budget.json").read_text(encoding="utf-8"))
        self.assertEqual((ledger["used_millicny"], ledger["http_attempts"], ledger["rounds"]), (0, 0, 1))
        self.assertIsNone(ledger["pending"])

    def test_non_budget_partial_run_pauses(self):
        receipt = self.receipt("run-partial", exit_code=9)
        receipt["code"] = "PARTIAL"
        receipt["summary"]["review"]["not_started"] = 1
        receipt["summary"]["scopes"] = [{"failure_code": "IO_ERROR"}]
        runner, _ = self.runner(receipt, returncode=9)
        self.assertNotEqual(self.mod.run_once(self.root, runner=runner), 0)
        self.assertTrue((self.root / "state/pause.request").is_file())


if __name__ == "__main__":
    unittest.main()
