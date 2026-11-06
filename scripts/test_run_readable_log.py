import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("run-readable-log.py")


class ReadableRunLogTest(unittest.TestCase):
    def test_run_writes_a_compact_auditable_log(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            fake = root / "dustkeeper"
            fake.write_text(
                textwrap.dedent(
                    """\
                    #!/usr/bin/env python3
                    import json, os, sys
                    args = sys.argv[1:]
                    with open(os.environ["DUSTKEEPER_CALL_LOG"], "a") as calls:
                        calls.write(" ".join(args) + "\\n")
                    if args[:2] == ["cleanup", "report"]:
                        page = {
                            "run_id": "run_fixture", "window_start_ns": 100, "window_end_ns": 200,
                            "deleted": {"total": 2, "entries": [{"item_id": "del", "path": "old/releases/a.zip", "size_bytes": 18, "reason": "dual_review_agrees", "reason_text": "双模型同意永久删除", "http_attempts": 2, "cost_status": "usage_recorded"}]},
                            "to_be_deleted": {"total": 1, "entries": [{"item_id": "q1", "path": "releases/old/application.jar", "size_bytes": 4096, "reason": "dual_review_agrees", "reason_text": "双模型同意进入观察区", "purge_after_ns": 1762000000000000000, "http_attempts": 2, "cost_status": "usage_recorded"}]},
                            "kept": {"total": 1, "entries": [{"item_id": "keep", "path": "logs/messages", "size_bytes": 19, "reason": "not_started_budget", "reason_text": "审核预算已用完，本轮未开始审核", "http_attempts": 0, "cost_status": "not_sent", "review_state": "not_started_budget"}]},
                            "review_summary": {"reviewed": 1, "review_not_started": 1, "review_unknown_held": 1,
                                               "review_failed_kept": 1, "http_attempts": 3, "cost_unknown": 1},
                        }
                        if os.environ.get("DUSTKEEPER_FAKE_FAIL"):
                            page["run_id"] = "run_failed"
                            page["kept"] = {"total": 0, "entries": []}
                        print(json.dumps({"status":"ok", "code":"OK", "data":{"page":json.dumps(page)}}))
                    elif args and args[0] == "events":
                        item = args[args.index("--item") + 1]
                        if item == "del":
                            old = {"decision":{"verdict":"keep", "jev":{"choice":"keep"}}}
                            audit = {"decision":{"verdict":"allow", "reason":"dual_review_agrees", "jev_model":"jev-test", "llm_model":"mm-test", "llm_consulted":True, "llm_decision":"allow", "llm_reason":"disposable", "llm_explanation":"Measured temporary package is replaceable.", "missing_evidence":["current_use"], "jev":{"choice":"allow_review", "confidence":0.95, "min_confidence":0.9, "min_allow_probability":0.9, "probabilities":{"allow_review":0.96}, "gate_passed":True, "blockers":["none"]}, "request_timings":[{"provider":"jev", "outcome":"ok", "elapsed_ms":120}]}}
                            future = {"decision":{"verdict":"keep", "jev":{"choice":"keep"}}}
                            events = [{"Action":"delete", "Phase":"intent", "OccurredAtNS":50, "ResultJSON":json.dumps(old)}, {"Action":"delete", "Phase":"intent", "OccurredAtNS":150, "ResultJSON":json.dumps(audit)}, {"Action":"delete", "Phase":"intent", "OccurredAtNS":250, "ResultJSON":json.dumps(future)}]
                        else:
                            events = [{"Action":"keep", "Phase":"info", "OccurredAtNS":50, "ResultJSON":json.dumps({"verdict":"insufficient", "model_error":""})}]
                        print(json.dumps({"status":"ok", "code":"OK", "data":{"events":events}}))
                    elif args and args[0] == "run":
                        if os.environ.get("DUSTKEEPER_FAKE_INVALID"):
                            print("not-json")
                            raise SystemExit(0)
                        if os.environ.get("DUSTKEEPER_FAKE_FAIL"):
                            print(json.dumps({"status":"failed", "code":"IO_ERROR", "message":"no file was moved or deleted after the failure", "data":{"run_id":"run_failed", "scopes":[{"scope_id":"server", "scanned_files":4, "purged":1, "purged_logical_bytes":18}], "failures":["review timeout"]}}))
                            raise SystemExit(7)
                        data = {"run_id":"run_fixture", "scopes":[{"scope_id":"server", "scanned_files":4, "scan_epoch_complete":True, "quarantined":0, "purged":1}], "disk_free_bytes_before":1000, "disk_free_bytes_after":1200, "control":"continue", "failures":[]}
                        print(json.dumps({"status":"ok", "code":"OK", "message":"", "data":data, "next_steps":[]}))
                    else:
                        raise SystemExit(2)
                    """
                ),
                encoding="utf-8",
            )
            fake.chmod(0o755)
            call_log = root / "calls.log"
            env = dict(os.environ, DUSTKEEPER_CALL_LOG=str(call_log))
            out = root / "reports"
            out.mkdir()
            for index in range(11):
                old_md = out / ("result-old-%02d-readable.md" % index)
                old_json = out / ("result-old-%02d-readable.json" % index)
                old_md.write_text("old", encoding="utf-8")
                old_json.write_text("{}", encoding="utf-8")
                os.utime(old_md, (index + 1, index + 1))
                os.utime(old_json, (index + 1, index + 1))
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--dustkeeper", str(fake), "--output-dir", str(out), "--disk-path", str(root)],
                text=True,
                capture_output=True,
                env=env,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            latest = (out / "latest-readable.md").read_text(encoding="utf-8")
            for expected in (
                "正式清理（双模型门禁）",
                "扫描文件：4",
                "候选：4",
                "已删除 2 / 待删除 1 / 保留 1",
                "删除逻辑大小：18 B",
                "old/releases/a.zip",
                "JEV `allow_review`（jev-test）",
                "MiniMax `allow`（mm-test）",
                "JEV 门禁：confidence 0.950 / 0.900；allow_review 0.960 / 0.900；通过",
                "JEV 阻碍：none",
                "MiniMax 简短理由：disposable；Measured temporary package is replaceable.",
                "缺失依据：current_use",
                "jev 120 ms (ok)",
                "logs/messages",
                "审批预算用完，本轮未发起请求；该对象不产生请求费用",
                "## 审核情况",
                "已取得判断：1 个",
                "未开始（预算用完，未发出请求）：1 个，不产生请求费用",
                "请求未取得结果（文件未动）：1 个",
                "上次请求结果未知、本轮未重复请求：1 个",
                "费用未知（不能按 0 计）：1 个",
                "本窗口 HTTP 尝试：3 次",
                "结果页共 4 条，这里展示 3 条，还有 1 条未展示",
                "请求事实：发出 2 次；费用状态 `usage_recorded`（未知不等于 0）",
                # V8: the observation section states what was done, until when the
                # object may stay, and how to take it back.
                "动作：本地移动到隔离区（quarantine），尚未永久删除",
                "最早清空：",
                "这是最早可清空时间，不是清空执行时间",
                "dustkeeper quarantine restore --object q1",
                "动作：已确认完成的永久删除；这个对象没有恢复入口",
                "动作：文件未移动、未删除，保持原位",
                # V10: an isolation is not a release; the deleted logical size
                # stays what the run actually purged.
                "删除逻辑大小：18 B",
            ):
                self.assertIn(expected, latest)
            self.assertNotIn("raw_response", latest)
            self.assertNotIn("prompt", latest)
            receipt = json.loads((out / "latest-readable.json").read_text(encoding="utf-8"))
            self.assertEqual(receipt["schema_version"], 2)
            self.assertEqual(receipt["summary"]["review"]["not_started"], 1)
            self.assertEqual(receipt["summary"]["review"]["http_attempts"], 3)
            self.assertEqual(receipt["summary"]["total_entries"], 4)
            self.assertEqual(receipt["summary"]["listed_entries"], 3)
            self.assertEqual(receipt["summary"]["omitted_entries"], 1)
            # Totals come from the page, not from the truncated list, so the
            # same action cannot be counted twice when a snapshot is re-read.
            self.assertEqual(receipt["summary"]["deleted"], 2)
            self.assertEqual(receipt["summary"]["observed"], 1)
            quarantined = [e for e in receipt["entries"] if e["outcome"] == "待删除"][0]
            self.assertEqual(quarantined["item_id"], "q1")
            self.assertTrue(quarantined["purge_after"].startswith("20"))
            self.assertTrue((out / "latest-readable.json").is_file())
            self.assertTrue((out / "result-run_fixture-readable.md").is_file())
            self.assertEqual(len(list(out.glob("result-*-readable.md"))), 10)
            self.assertEqual(len(list(out.glob("result-*-readable.json"))), 10)

            captured = root / "run.json"
            captured.write_text(json.dumps({"status":"ok", "code":"OK", "message":"", "data":{"run_id":"run_fixture", "scopes":[], "disk_free_bytes_before":1000, "disk_free_bytes_after":1200, "control":"continue"}}), encoding="utf-8")
            formatted = subprocess.run(
                [sys.executable, str(SCRIPT), "--dustkeeper", str(fake), "--output-dir", str(root / "formatted"), "--disk-path", str(root), "--run-json", str(captured)],
                text=True,
                capture_output=True,
                env=env,
            )
            self.assertEqual(formatted.returncode, 0, formatted.stderr)
            calls = call_log.read_text(encoding="utf-8").splitlines()
            self.assertEqual(sum(line.startswith("run ") for line in calls), 1, calls)

            failed_out = root / "failed"
            failed = subprocess.run(
                [sys.executable, str(SCRIPT), "--dustkeeper", str(fake), "--output-dir", str(failed_out), "--disk-path", str(root)],
                text=True,
                capture_output=True,
                env=dict(env, DUSTKEEPER_FAKE_FAIL="1"),
            )
            self.assertEqual(failed.returncode, 7)
            failed_log = (failed_out / "latest-readable.md").read_text(encoding="utf-8")
            self.assertIn("IO_ERROR", failed_log)
            self.assertIn("本轮状态：部分完成", failed_log)
            self.assertIn("已删除 2 个文件", failed_log)
            self.assertIn("不能理解为整轮零操作", failed_log)
            self.assertIn("old/releases/a.zip", failed_log)

            invalid_out = root / "invalid"
            invalid = subprocess.run(
                [sys.executable, str(SCRIPT), "--dustkeeper", str(fake), "--output-dir", str(invalid_out), "--disk-path", str(root)],
                text=True,
                capture_output=True,
                env=dict(env, DUSTKEEPER_FAKE_INVALID="1"),
            )
            self.assertNotEqual(invalid.returncode, 0)
            self.assertIn("INVALID_OUTPUT", (invalid_out / "latest-readable.md").read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
