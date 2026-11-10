#!/usr/bin/env python3
"""Run one bounded scheduled Dustkeeper round and keep a durable cost ledger."""

import argparse
import fcntl
import json
import os
import subprocess
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path


RESERVE_MILLICNY = 10
REPORT_SCHEMA = 3
MAX_JSON = 4 << 20


class Refused(Exception):
    pass


def _now_ns():
    return int(time.time() * 1000000000)


def _read_json(path):
    if not path.is_file() or path.stat().st_size > MAX_JSON:
        raise Refused("required JSON is missing or oversized")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise Refused("required JSON is invalid")
    if not isinstance(value, dict):
        raise Refused("required JSON is not an object")
    return value


def _atomic_json(path, value):
    fd, tmp = tempfile.mkstemp(prefix=".automation-budget-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, sort_keys=True, separators=(",", ":"))
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, str(path))
        directory = os.open(str(path.parent), os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _integer(value, name):
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise Refused("ledger field %s is invalid" % name)
    return value


def _ledger(path):
    data = _read_json(path)  # Missing is a refusal: installation creates it explicitly.
    if data.get("schema") != 1 or not isinstance(data.get("models"), dict):
        raise Refused("ledger schema is invalid")
    for field in ("limit_millicny", "used_millicny", "rounds", "http_attempts"):
        _integer(data.get(field), field)
    if not isinstance(data.get("history"), list) or data.get("pending") is not None and not isinstance(data.get("pending"), dict):
        raise Refused("ledger state is invalid")
    if data["used_millicny"] > data["limit_millicny"]:
        raise Refused("ledger is already over its limit")
    return data


def _validate_instance(root, ledger):
    config = _read_json(root / "config/config.json")
    review = config.get("review")
    if not isinstance(review, dict):
        raise Refused("dual review is not configured")
    expected = {"max_candidates": 1, "batch_max_candidates": 1, "batch_max_attempts": 2}
    if any(review.get(k) != v for k, v in expected.items()):
        raise Refused("review batch limits do not match scheduled approval")
    models = ledger["models"]
    if review.get("jev_model") != models.get("jev") or review.get("minimax_model") != models.get("minimax"):
        raise Refused("review models differ from the installed ledger")
    credentials = _read_json(Path(str(review.get("credentials_file", ""))))
    if not str(credentials.get("typesafe_api_key", "")).strip():
        raise Refused("JEV credential is missing")
    if not str(credentials.get("minimax_api_key", "")).startswith("sk-cp-"):
        raise Refused("MiniMax credential is not the approved subscription-key format")
    binary, wrapper = root / "bin/dustkeeper", root / "run-readable-log.py"
    if not binary.is_file() or not os.access(str(binary), os.X_OK) or not wrapper.is_file():
        raise Refused("installed binary or readable-log wrapper is missing")
    return binary, wrapper


def _default_runner(cmd, env, timeout):
    return subprocess.run(cmd, env=env, timeout=timeout, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, universal_newlines=True, check=False)


def _pause(root, binary, runner):
    env = dict(os.environ, DUSTKEEPER_CONFIG_DIR=str(root / "config"),
               DUSTKEEPER_STATE_DIR=str(root / "state"))
    try:
        runner([str(binary), "pause", "--json"], env, 60)
    except Exception:
        pass
    return (root / "state/pause.request").is_file()


def _snapshot(path):
    if not path.is_file():
        return {"run_id": "", "ended_at": "", "mtime_ns": 0}
    value = _read_json(path)
    return {"run_id": str(value.get("run_id", "")), "ended_at": str(value.get("ended_at", "")),
            "mtime_ns": path.stat().st_mtime_ns}


def _parse_time(value):
    if not isinstance(value, str):
        raise Refused("readable receipt time is missing")
    text = value
    if text.endswith("Z"):
        text = text[:-1] + "+0000"
    elif len(text) >= 6 and text[-3] == ":":
        text = text[:-3] + text[-2:]
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%S%z").timestamp()
    except (TypeError, ValueError):
        raise Refused("readable receipt time is invalid")


def _receipt(path, before, completed, launched_ns):
    current = _read_json(path)
    review = current.get("summary", {}).get("review", {})
    if current.get("schema_version") != REPORT_SCHEMA or not isinstance(review, dict):
        raise Refused("readable receipt schema is invalid")
    run_id = str(current.get("run_id", ""))
    started, ended = current.get("started_at"), current.get("ended_at")
    started_s, ended_s = _parse_time(started), _parse_time(ended)
    if not run_id or not ended or run_id == before["run_id"] or ended == before["ended_at"] or path.stat().st_mtime_ns <= before["mtime_ns"]:
        raise Refused("readable receipt is not from this round")
    if started_s < launched_ns / 1000000000.0 - 1 or ended_s < started_s or ended_s < launched_ns / 1000000000.0 - 1:
        raise Refused("readable receipt time is outside this round")
    exit_code = current.get("exit")
    attempts = review.get("http_attempts")
    if isinstance(attempts, bool) or not isinstance(attempts, int) or attempts < 0 or attempts > 2:
        raise Refused("HTTP attempt count is missing or exceeds the approved batch")
    bad = ("review_failed", "unknown_held", "cost_unknown", "facts_unknown")
    if any(name not in review or isinstance(review.get(name), bool) or
           not isinstance(review.get(name), int) or review.get(name) != 0 for name in bad):
        raise Refused("review receipt contains an unresolved or unknown result")
    if isinstance(exit_code, bool) or not isinstance(exit_code, int) or exit_code not in (0, 9) or completed.returncode != exit_code:
        raise Refused("run did not finish with an accepted receipt")
    if exit_code == 9:
        summary = current["summary"]
        not_started = review.get("not_started")
        if (current.get("code") not in ("BUDGET_EXHAUSTED", "PARTIAL") or
                type(not_started) is not int or not_started < 1 or
                any(row.get("failure_code", "") not in ("", "BUDGET_EXHAUSTED")
                    for row in summary.get("scopes", []))):
            raise Refused("partial run was not solely a bounded review-budget stop")
    reviewed = review.get("reviewed")
    if attempts > 0 and (isinstance(reviewed, bool) or not isinstance(reviewed, int) or reviewed < 1):
        raise Refused("model usage or review settlement is missing")
    return current, attempts


def run_once(instance_root, runner=None):
    root = Path(instance_root)
    if not root.is_absolute():
        raise Refused("--instance-root must be absolute")
    runner = runner or _default_runner
    lock_fd = os.open(str(root / "automation-budget.lock"), os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except IOError:
        os.close(lock_fd)
        return 2
    try:
        ledger_path = root / "automation-budget.json"
        binary = root / "bin/dustkeeper"
        ledger = _ledger(ledger_path)
        binary, wrapper = _validate_instance(root, ledger)
        if (root / "state/pause.request").exists():
            print("REFUSED: instance is already paused")
            return 2
        if ledger["pending"] is not None:
            ledger["pending"]["pause_confirmed"] = _pause(root, binary, runner)
            _atomic_json(ledger_path, ledger)
            print("REFUSED: previous round is pending; pause_confirmed=%s" % ledger["pending"]["pause_confirmed"])
            return 3 if ledger["pending"]["pause_confirmed"] else 6
        if ledger["used_millicny"] + RESERVE_MILLICNY > ledger["limit_millicny"]:
            paused = _pause(root, binary, runner)
            print("REFUSED: automation budget exhausted; pause_confirmed=%s" % paused)
            return 4 if paused else 6
        report = root / "readable-reports/latest-readable.json"
        before = _snapshot(report)
        ledger["used_millicny"] += RESERVE_MILLICNY
        ledger["pending"] = {"reserved_millicny": RESERVE_MILLICNY, "started_ns": _now_ns(),
                             "previous_run_id": before["run_id"]}
        _atomic_json(ledger_path, ledger)
        env = dict(os.environ, DUSTKEEPER_CONFIG_DIR=str(root / "config"),
                   DUSTKEEPER_STATE_DIR=str(root / "state"))
        cmd = [sys.executable, str(wrapper), "--dustkeeper", str(binary), "--output-dir",
               str(root / "readable-reports"), "--disk-path", "/", "--timeout-seconds", "900"]
        try:
            completed = runner(cmd, env, 960)
            receipt, attempts = _receipt(report, before, completed, ledger["pending"]["started_ns"])
        except Exception as error:
            ledger = _ledger(ledger_path)
            ledger["pending"]["failure"] = type(error).__name__
            ledger["pending"]["pause_confirmed"] = _pause(root, binary, runner)
            _atomic_json(ledger_path, ledger)
            print("BLOCKED: run receipt rejected; pause_confirmed=%s" % ledger["pending"]["pause_confirmed"])
            return 5 if ledger["pending"]["pause_confirmed"] else 6
        ledger = _ledger(ledger_path)
        if attempts == 0:
            ledger["used_millicny"] -= RESERVE_MILLICNY
        ledger["rounds"] += 1
        ledger["http_attempts"] += attempts
        ledger["history"] = (ledger["history"] + [{"run_id": receipt["run_id"],
            "ended_at": receipt["ended_at"], "exit": receipt["exit"], "http_attempts": attempts,
            "reserved_millicny": 0 if attempts == 0 else RESERVE_MILLICNY}])[-10:]
        ledger["pending"] = None
        _atomic_json(ledger_path, ledger)
        print(json.dumps({"status": "ok", "run_id": receipt["run_id"], "exit": receipt["exit"],
                          "http_attempts": attempts, "reserved_millicny": 0 if attempts == 0 else RESERVE_MILLICNY},
                         separators=(",", ":")))
        return 0
    except Refused as error:
        if (root / "state/pause.request").exists():
            print("REFUSED: %s; instance already paused" % error)
            return 2
        paused = _pause(root, root / "bin/dustkeeper", runner)
        print("REFUSED: %s; pause_confirmed=%s" % (error, paused))
        return 2 if paused else 6
    finally:
        fcntl.flock(lock_fd, fcntl.LOCK_UN)
        os.close(lock_fd)


def main():
    parser = argparse.ArgumentParser(description="run one bounded scheduled Dustkeeper receipt")
    parser.add_argument("--instance-root", required=True)
    args = parser.parse_args()
    try:
        return run_once(Path(args.instance_root))
    except Exception:
        return 2


if __name__ == "__main__":
    sys.exit(main())
