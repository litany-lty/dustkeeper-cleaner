#!/usr/bin/env python3
"""Run Dustkeeper once and write a compact, private, human-readable receipt."""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime
from pathlib import Path


MAX_OUTPUT_BYTES = 8 << 20
# One snapshot directory is bounded. "Keep ten runs" without a byte ceiling is
# not a limit: ten runs of a large clean-up can fill the volume the clean-up was
# supposed to free.
MAX_REPORT_DIR_BYTES = 20 << 20
# The result page shows at most 200 entries per section; the readable log keeps
# the same ceiling and states the total and the remainder.
MAX_LISTED_ENTRIES = 200
SCHEMA_VERSION = 2


def parse_args():
    parser = argparse.ArgumentParser(
        description="run the installed Dustkeeper once and automatically write a readable receipt"
    )
    parser.add_argument("--dustkeeper", required=True, help="installed Dustkeeper executable")
    parser.add_argument("--output-dir", required=True, help="private directory outside the cleanup scopes")
    parser.add_argument("--disk-path", default="/", help="filesystem used for before/after free-space readings")
    parser.add_argument("--timeout-seconds", type=int, default=900, help="single-run deadline; no retry")
    parser.add_argument("--run-json", help="format one already captured `dustkeeper run --json` envelope; do not run again")
    args = parser.parse_args()
    if not Path(args.dustkeeper).is_absolute() or not Path(args.output_dir).is_absolute() or not Path(args.disk_path).is_absolute():
        parser.error("--dustkeeper, --output-dir and --disk-path must be absolute paths")
    if args.timeout_seconds < 1 or args.timeout_seconds > 7200:
        parser.error("--timeout-seconds must be between 1 and 7200")
    return args


def load_run_envelope(path):
    source = Path(path)
    if not source.is_absolute() or not source.is_file() or source.stat().st_size > MAX_OUTPUT_BYTES:
        raise SystemExit("--run-json must be an absolute JSON file no larger than 8 MiB")
    try:
        envelope = json.loads(source.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise SystemExit("--run-json is not a readable Dustkeeper JSON envelope")
    if not isinstance(envelope, dict) or not isinstance(envelope.get("data"), dict):
        raise SystemExit("--run-json does not contain a run data object")
    return envelope


def invoke(executable, args, timeout):
    try:
        completed = subprocess.run(
            [executable, *args, "--json"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            universal_newlines=True,
            timeout=timeout,
            check=False,
        )
    except subprocess.TimeoutExpired:
        return 130, {"status": "cancelled", "code": "CANCELLED", "message": "运行超过本次时限，未重试"}, ""
    stdout = completed.stdout
    if len(stdout.encode("utf-8", errors="replace")) > MAX_OUTPUT_BYTES:
        return 7, {"status": "failed", "code": "OUTPUT_TOO_LARGE", "message": "命令输出超过 8 MiB，未保存原始输出"}, ""
    try:
        envelope = json.loads(stdout)
        if not isinstance(envelope, dict):
            raise TypeError("the envelope is not an object")
    except (TypeError, json.JSONDecodeError):
        envelope = {
            "status": "failed",
            "code": "INVALID_OUTPUT",
            "message": "Dustkeeper 未返回可解析的 JSON；原始输出未写入日志",
        }
        if completed.returncode == 0:
            completed = subprocess.CompletedProcess(completed.args, 7, completed.stdout, completed.stderr)
    stderr = "命令有额外诊断输出；为避免泄露原始响应或凭据，未写入本日志" if completed.stderr.strip() else ""
    return completed.returncode, envelope, stderr


def page_from(executable, timeout):
    code, envelope, _ = invoke(executable, ["cleanup", "report"], timeout)
    if code != 0:
        return {}
    raw = envelope.get("data", {}).get("page", "")
    if not isinstance(raw, str):
        return {}
    try:
        page = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    return page if isinstance(page, dict) else {}


def event_payload(value):
    if isinstance(value, dict):
        return value
    if not isinstance(value, str):
        return {}
    try:
        parsed = json.loads(value)
    except json.JSONDecodeError:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def review_for(executable, item_id, reason, timeout, window_start_ns, window_end_ns):
    if not item_id:
        return review_summary({}, reason, False)
    code, envelope, _ = invoke(executable, ["events", "--item", item_id], timeout)
    if code != 0:
        return review_summary({}, reason, False)
    events = envelope.get("data", {}).get("events", [])
    decision = {}
    if isinstance(events, list):
        for event in reversed(events):
            if not isinstance(event, dict):
                continue
            occurred = int(event.get("OccurredAtNS", event.get("occurred_at_ns", 0)) or 0)
            if occurred < window_start_ns or occurred > window_end_ns:
                continue
            phase = event.get("Phase", event.get("phase", ""))
            payload = event_payload(event.get("ResultJSON", event.get("result_json")))
            candidate = payload.get("decision", payload if phase == "info" else {})
            if isinstance(candidate, dict) and candidate:
                decision = candidate
                break
    return review_summary(decision, reason, bool(decision))


def review_summary(decision, reason, recorded):
    jev = decision.get("jev") if isinstance(decision.get("jev"), dict) else {}
    jev_choice = jev.get("choice", "")
    minimax = decision.get("llm_decision", "")
    consulted = bool(decision.get("llm_consulted", False))
    if reason == "dual_review_agrees":
        jev_choice = jev_choice or "allow_review"
        minimax = minimax or "allow"
        consulted = True
    timings = []
    for row in decision.get("request_timings", []):
        if not isinstance(row, dict):
            continue
        timings.append(
            {
                "provider": str(row.get("provider", "unknown")),
                "outcome": str(row.get("outcome", "unknown")),
                "phase": str(row.get("phase", "")),
                "elapsed_ms": int(row.get("elapsed_ms", 0) or 0),
            }
        )
    return {
        "verdict": str(decision.get("verdict", "")),
        "reason": str(decision.get("reason", "")),
        "model_error": str(decision.get("model_error", "")),
        "jev_decision": str(jev_choice),
        "jev_model": str(decision.get("jev_model", "")),
        "jev_confidence": safe_float(jev.get("confidence")),
        "jev_min_confidence": safe_float(jev.get("min_confidence")),
        "jev_allow_probability": safe_float(jev.get("probabilities", {}).get("allow_review")) if isinstance(jev.get("probabilities"), dict) else None,
        "jev_min_allow_probability": safe_float(jev.get("min_allow_probability")),
        "jev_gate_passed": bool(jev.get("gate_passed", False)),
        "jev_blockers": safe_list(jev.get("blockers"), 8, 80),
        "minimax_decision": str(minimax),
        "minimax_model": str(decision.get("llm_model", "")),
        "minimax_consulted": consulted,
        "minimax_reason": safe_text(decision.get("llm_reason", ""), 300),
        "minimax_explanation": safe_text(decision.get("llm_explanation", ""), 300),
        "missing_evidence": safe_list(decision.get("missing_evidence"), 8, 80),
        "current_run_recorded": recorded,
        "timings": timings,
        "jev_usage": safe_usage(jev.get("usage")),
        "minimax_usage": safe_usage(decision.get("llm_usage")),
    }


def safe_text(value, limit):
    text = str(value or "").replace("\r", " ").replace("\n", " ").strip()
    return text if len(text) <= limit else text[:limit] + "…[truncated]"


def safe_list(value, count, width):
    if not isinstance(value, list):
        return []
    return [safe_text(item, width) for item in value[:count] if safe_text(item, width)]


def safe_float(value):
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def safe_usage(value):
    if not isinstance(value, dict):
        return None
    return {
        "input_tokens": int(value.get("input_tokens", 0) or 0),
        "output_tokens": int(value.get("output_tokens", 0) or 0),
    }


def iso_from_ns(value):
    """Format a nanosecond timestamp the same way the rest of the receipt does."""
    if value is None:
        return ""
    try:
        seconds = int(value) / 1_000_000_000.0
    except (TypeError, ValueError):
        return ""
    try:
        return datetime.fromtimestamp(seconds).astimezone().isoformat(timespec="seconds")
    except (OverflowError, OSError, ValueError):
        return ""


def collect_entries(executable, page, timeout):
    rows = []
    window_start_ns = int(page.get("window_start_ns", 0) or 0)
    window_end_ns = int(page.get("window_end_ns", 0) or 0)
    for outcome, key in (("已删除", "deleted"), ("待删除", "to_be_deleted"), ("保留", "kept")):
        section = page.get(key, {})
        entries = section.get("entries", []) if isinstance(section, dict) else []
        for entry in entries if isinstance(entries, list) else []:
            if not isinstance(entry, dict):
                continue
            reason = str(entry.get("reason", ""))
            recorded = review_for(executable, str(entry.get("item_id", "")), reason, timeout, window_start_ns, window_end_ns)
            # The page's own record is authoritative for the request facts; the
            # event lookup is a fallback for older pages that do not carry them.
            attempts = entry.get("http_attempts")
            if isinstance(attempts, int):
                recorded["attempts"] = attempts
            else:
                recorded["attempts"] = None
            recorded["page_cost_status"] = str(entry.get("cost_status", ""))
            recorded["page_review_state"] = str(entry.get("review_state", ""))
            rows.append(
                {
                    "outcome": outcome,
                    "path": str(entry.get("path", "")),
                    "size_bytes": int(entry.get("size_bytes", 0) or 0),
                    "reason": reason,
                    "reason_text": str(entry.get("reason_text", "")),
                    # The recovery entry point and the earliest clear time are
                    # part of "what happened", not a separate lookup: a receipt
                    # that says "waiting to be deleted" without saying until when
                    # it may stay, or how to take it back, cannot be acted on.
                    "item_id": str(entry.get("item_id", "")),
                    "scope_id": str(entry.get("scope_id", "")),
                    "purge_after": iso_from_ns(entry.get("purge_after_ns")),
                    "review": recorded,
                }
            )
    return rows


def section_total(page, key):
    section = page.get(key, {})
    return int(section.get("total", 0) or 0) if isinstance(section, dict) else 0


def build_receipt(
    run_code: int,
    envelope,
    page,
    entries,
    started,
    ended,
    disk_path,
    free_before,
    free_after,
    stderr,
):
    run = envelope.get("data", {}) if isinstance(envelope.get("data"), dict) else {}
    scopes = run.get("scopes", []) if isinstance(run.get("scopes"), list) else []
    scanned = sum(int(s.get("scanned_files", 0) or 0) for s in scopes if isinstance(s, dict))
    deleted = section_total(page, "deleted")
    observed = section_total(page, "to_be_deleted")
    kept = section_total(page, "kept")
    deleted_bytes = sum(int(s.get("purged_logical_bytes", 0) or 0) for s in scopes if isinstance(s, dict))
    if deleted_bytes == 0:
        deleted_bytes = sum(row["size_bytes"] for row in entries if row["outcome"] == "已删除")
    scope_rows = []
    for scope in scopes:
        if not isinstance(scope, dict):
            continue
        scope_rows.append({
            "scope_id": str(scope.get("scope_id", "")),
            "scanned_files": int(scope.get("scanned_files", 0) or 0),
            "scan_complete": bool(scope.get("scan_epoch_complete", False)),
            "deleted": int(scope.get("purged", 0) or 0),
            "observed": int(scope.get("quarantined", 0) or 0),
            "failure_code": str(scope.get("failure_code", "")),
            "skipped": str(scope.get("skipped", "")),
            "purge_skipped": str(scope.get("purge_skipped", "")),
            "quarantine_skipped": str(scope.get("quarantine_skipped", "")),
        })
    review_summary = page.get("review_summary", {}) if isinstance(page.get("review_summary"), dict) else {}
    kept_sections = {
        "deleted": page.get("deleted", {}),
        "to_be_deleted": page.get("to_be_deleted", {}),
        "kept": page.get("kept", {}),
    }
    return {
        "schema_version": SCHEMA_VERSION,
        "artifact_schema": "current",
        "mode": "正式清理（双模型门禁）",
        "goal": "按已确认策略处理已登记范围；模型意见不扩大授权",
        "started_at": started,
        "ended_at": ended,
        "run_id": str(run.get("run_id", page.get("run_id", ""))),
        "status": str(envelope.get("status", "failed")),
        "code": str(envelope.get("code", "UNKNOWN")),
        "exit": run_code,
        "message": str(envelope.get("message", "")),
        "next_steps": [str(x) for x in envelope.get("next_steps", []) if isinstance(x, str)],
        "stderr_summary": stderr,
        "summary": {
            "scanned_files": scanned,
            "candidate_files": deleted + observed + kept,
            "deleted": deleted,
            "observed": observed,
            "kept": kept,
            "deleted_logical_bytes": deleted_bytes,
            "control": str(run.get("control", "")),
            "partial_completed": (run_code != 0 or envelope.get("code") != "OK") and (deleted > 0 or observed > 0),
            "failures": [str(x) for x in run.get("failures", []) if isinstance(x, str)],
            "review": {
                "reviewed": int(review_summary.get("reviewed", 0) or 0) if review_summary else None,
                "not_started": int(review_summary.get("review_not_started", 0) or 0) if review_summary else None,
                "unknown_held": int(review_summary.get("review_unknown_held", 0) or 0) if review_summary else None,
                "review_failed": int(review_summary.get("review_failed_kept", 0) or 0) if review_summary else None,
                "http_attempts": int(review_summary.get("http_attempts", 0) or 0) if review_summary else None,
                "cost_unknown": int(review_summary.get("cost_unknown", 0) or 0) if review_summary else None,
                "run_not_started": int(run.get("review_not_started_total", 0) or 0) if "review_not_started_total" in run else None,
                "run_unknown_held": int(run.get("review_unknown_held_total", 0) or 0) if "review_unknown_held_total" in run else None,
            },
            "listed_entries": len(entries),
            "total_entries": sum(int(section.get("total", 0) or 0) for section in kept_sections.values() if isinstance(section, dict)),
            "omitted_entries": max(0, sum(int(section.get("total", 0) or 0) for section in kept_sections.values() if isinstance(section, dict)) - len(entries)),
            "timezone": "Asia/Shanghai (local offset recorded in each timestamp)",
            "scopes": scope_rows,
        },
        "disk": {
            "path": disk_path,
            "free_before": int(run.get("disk_free_bytes_before", 0) or free_before),
            "free_after": int(run.get("disk_free_bytes_after", 0) or free_after),
            "wrapper_free_before": free_before,
            "wrapper_free_after": free_after,
        },
        "entries": entries,
    }


def human_bytes(value):
    sign = "-" if value < 0 else ""
    size = float(abs(value))
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if size < 1024 or unit == "TiB":
            return f"{sign}{int(size)} B" if unit == "B" else f"{sign}{size:.1f} {unit}"
        size /= 1024
    return f"{sign}{size:.1f} PiB"


def md_code(value):
    value = value.replace("\r", " ").replace("\n", " ")
    longest = 0
    run = 0
    for char in value:
        if char == "`":
            run += 1
            longest = max(longest, run)
        else:
            run = 0
    fence = "`" * (longest + 1)
    return f"{fence} {value} {fence}" if longest else f"{fence}{value}{fence}"


def render(receipt):
    summary = receipt["summary"]
    disk = receipt["disk"]
    lines = [
        "# Dustkeeper 本轮运行日志",
        "",
        f"- 目标：{receipt['goal']}",
        f"- 模式：{receipt['mode']}",
        f"- 时间：{receipt['started_at']} 至 {receipt['ended_at']}",
        f"- 运行编号：{md_code(receipt['run_id'] or '未生成')}",
        f"- 结果：{md_code(receipt['code'])}（exit {receipt['exit']}）",
        "",
        "## 本轮概览",
        "",
        f"扫描文件：{summary['scanned_files']}；候选：{summary['candidate_files']}；已删除 {summary['deleted']} / 待删除 {summary['observed']} / 保留 {summary['kept']}。",
        f"删除逻辑大小：{human_bytes(summary['deleted_logical_bytes'])}。这是已删除文件的逻辑大小，不冒充底层磁盘实际回收量。",
        f"磁盘可用空间：{human_bytes(disk['free_before'])} → {human_bytes(disk['free_after'])}（变化 {human_bytes(disk['free_after'] - disk['free_before'])}）。",
    ]
    if summary["partial_completed"]:
        # The sentence has to name what actually finished and what did not. "No
        # file was moved or deleted" was the wrong sentence here: ten deletions
        # had already completed when the eleventh request timed out.
        lines.append(
            f"本轮状态：部分完成。已删除 {summary['deleted']} 个文件，"
            f"进入待删除 {summary['observed']} 个；失败原因只对应尚未执行或未完成的对象，"
            "不能理解为整轮零操作。"
        )
    review = summary.get("review") or {}
    if review and (review.get("not_started") or review.get("unknown_held")
                   or review.get("review_failed") or review.get("cost_unknown")):
        lines.extend(["", "## 审核情况", ""])
        if review.get("reviewed") is not None:
            lines.append(f"已取得判断：{review['reviewed']} 个")
        if review.get("not_started"):
            lines.append(f"未开始（预算用完，未发出请求）：{review['not_started']} 个，不产生请求费用")
        if review.get("review_failed"):
            lines.append(f"请求未取得结果（文件未动）：{review['review_failed']} 个")
        if review.get("unknown_held"):
            lines.append(f"上次请求结果未知、本轮未重复请求：{review['unknown_held']} 个")
        if review.get("cost_unknown"):
            lines.append(f"费用未知（不能按 0 计）：{review['cost_unknown']} 个")
        if review.get("http_attempts") is not None:
            lines.append(f"本窗口 HTTP 尝试：{review['http_attempts']} 次")
        if review.get("not_started") is None and review.get("review_failed") is None:
            lines.append("旧制品未记录审核计数")
    if summary["failures"] or receipt["message"] or receipt["stderr_summary"]:
        lines.extend(["", "## 失败与下一步", ""])
        if receipt["message"]:
            label = "未执行项说明（原命令）" if summary["partial_completed"] else "结果说明"
            lines.append(f"- {label}：{receipt['message']}")
        for failure in summary["failures"]:
            lines.append(f"- 失败阶段：{failure}")
        if receipt["stderr_summary"]:
            lines.append(f"- 诊断摘要：{receipt['stderr_summary']}")
        for step in receipt["next_steps"]:
            lines.append(f"- 下一步：{md_code(step)}")
    lines.extend(["", "## 逐候选判断", ""])
    total = summary.get("total_entries")
    listed = summary.get("listed_entries")
    omitted = summary.get("omitted_entries")
    if isinstance(total, int):
        note = f"结果页共 {total} 条，这里展示 {listed} 条"
        if omitted:
            note += f"，还有 {omitted} 条未展示"
        lines.append(note + "。")
        lines.append("")
    if not receipt["entries"]:
        lines.append("本轮没有进入删除、待删除或保留结果的候选。")
    for entry in receipt["entries"]:
        review = entry["review"]
        lines.extend(
            [
                f"### {entry['outcome']} · {md_code(entry['path'])}",
                "",
                f"- 大小：{human_bytes(entry['size_bytes'])}",
                f"- 依据：{entry['reason_text'] or entry['reason'] or '当前制品未记录'}",
            ]
        )
        # One line per outcome stating what was done to the file, so "waiting to
        # be deleted" is never read as "deleted", and the observation section
        # carries the two things a reader needs next: until when it stays, and
        # how to take it back.
        if entry["outcome"] == "已删除":
            lines.append("- 动作：已确认完成的永久删除；这个对象没有恢复入口")
        elif entry["outcome"] == "待删除":
            lines.append("- 动作：本地移动到隔离区（quarantine），尚未永久删除")
            lines.append(
                "- 最早清空：" + (entry["purge_after"] + "（这是最早可清空时间，不是清空执行时间）"
                                if entry["purge_after"] else "旧制品未记录")
            )
            if entry["item_id"]:
                lines.append("- 恢复：" + md_code("dustkeeper quarantine restore --object " + entry["item_id"]))
            else:
                lines.append("- 恢复：旧制品未记录对象标识；用 `dustkeeper quarantine list` 按路径查找")
        else:
            lines.append("- 动作：文件未移动、未删除，保持原位")
        attempts = review.get("attempts")
        page_state = review.get("page_review_state", "")
        if entry["reason"] == "not_started_budget" or page_state == "not_started_budget":
            lines.append("- 模型调用：审批预算用完，本轮未发起请求；该对象不产生请求费用")
            no_current_review = True
        elif attempts is None:
            lines.append("- 模型调用：旧制品未记录 HTTP 尝试次数")
            no_current_review = not review["current_run_recorded"]
        elif attempts == 0 and entry["reason"] in ("awaiting_request_check",):
            lines.append("- 模型调用：上次请求结果未知，本轮未再次请求；费用仍未知")
            no_current_review = True
        elif attempts == 0:
            lines.append("- 模型调用：本轮没有发起请求；结果来自缓存或本地门禁")
            no_current_review = True
        else:
            no_current_review = False
        if no_current_review:
            pass
        elif review["jev_decision"]:
            model = f"（{review['jev_model']}）" if review["jev_model"] else ""
            lines.append(f"- JEV {md_code(review['jev_decision'])}{model}")
            values = (review["jev_confidence"], review["jev_min_confidence"], review["jev_allow_probability"], review["jev_min_allow_probability"])
            if all(value is not None for value in values):
                gate = "通过" if review["jev_gate_passed"] else "未通过"
                lines.append(f"- JEV 门禁：confidence {values[0]:.3f} / {values[1]:.3f}；allow_review {values[2]:.3f} / {values[3]:.3f}；{gate}")
            if review["jev_blockers"]:
                lines.append("- JEV 阻碍：" + "；".join(review["jev_blockers"]))
        if no_current_review:
            pass
        elif review["minimax_consulted"] and review["minimax_decision"]:
            model = f"（{review['minimax_model']}）" if review["minimax_model"] else ""
            lines.append(f"- MiniMax {md_code(review['minimax_decision'])}{model}")
        elif review["verdict"]:
            sent = "已发出请求但未取得结果" if attempts else "MiniMax 未被调用"
            lines.append(f"- 综合结论 {md_code(review['verdict'])}；{sent}")
        else:
            lines.append("- 模型调用：当前制品未记录（可能由本地门禁直接保留）")
        minimax_notes = [note for note in (review["minimax_reason"], review["minimax_explanation"]) if note]
        if minimax_notes:
            lines.append("- MiniMax 简短理由：" + "；".join(minimax_notes))
        if review["missing_evidence"]:
            lines.append("- 缺失依据：" + "；".join(review["missing_evidence"]))
        if review["model_error"]:
            lines.append(f"- 模型错误：{review['model_error']}")
        if attempts:
            cost = review.get("page_cost_status") or "unknown"
            lines.append(f"- 请求事实：发出 {attempts} 次；费用状态 {md_code(cost)}（未知不等于 0）")
        if review["timings"]:
            timing = "；".join(
                f"{row['provider']} {row['elapsed_ms']} ms ({row['outcome']})" for row in review["timings"]
            )
            lines.append(f"- 网络耗时：{timing}")
        else:
            lines.append("- 网络耗时：旧制品或正式模式未记录分段时序")
        lines.append("")
    lines.extend(
        [
            "## 日志边界",
            "",
            "",
            "本文件不保存文件正文、模型提示词、原始响应、请求头、地址或凭据。完整结构化数据见同目录的 `latest-readable.json`。",
            "",
        ]
    )
    return "\n".join(lines)


def atomic_write(path, data):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    fd, name = tempfile.mkstemp(prefix=".dustkeeper-report-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(name, path)
    finally:
        try:
            os.unlink(name)
        except FileNotFoundError:
            pass


def prune_snapshots(output_dir, keep=10):
    """Keep the newest runs, bounded by both count and bytes.

    Only files this tool named are ever removed: a broad wildcard in a directory
    a person also uses is how a log rotation deletes someone else's document.
    """
    snapshots = sorted(
        output_dir.glob("result-*-readable.md"),
        key=lambda path: path.stat().st_mtime_ns,
        reverse=True,
    )
    for markdown in snapshots[keep:]:
        json_path = markdown.with_suffix(".json")
        markdown.unlink()
        try:
            json_path.unlink()
        except FileNotFoundError:
            pass
    files = sorted(
        [p for p in output_dir.glob("result-*-readable.*") if p.is_file()],
        key=lambda path: path.stat().st_mtime_ns,
    )
    total = sum(path.stat().st_size for path in files)
    for path in files:
        if total <= MAX_REPORT_DIR_BYTES:
            break
        try:
            size = path.stat().st_size
            path.unlink()
            total -= size
        except FileNotFoundError:
            continue


def main():
    args = parse_args()
    executable = Path(args.dustkeeper)
    if not executable.is_file() or not os.access(executable, os.X_OK):
        raise SystemExit("--dustkeeper must name an executable file")
    output_dir = Path(args.output_dir)
    disk_path = Path(args.disk_path)
    if not disk_path.exists():
        raise SystemExit("--disk-path does not exist")
    started = datetime.now().astimezone().isoformat(timespec="seconds")
    if args.run_json:
        envelope = load_run_envelope(args.run_json)
        code = 0 if envelope.get("code") == "OK" else 1
        stderr = ""
        run_data = envelope.get("data", {})
        free_before = int(run_data.get("disk_free_bytes_before", 0) or 0)
        free_after = int(run_data.get("disk_free_bytes_after", 0) or 0)
    else:
        free_before = shutil.disk_usage(disk_path).free
        code, envelope, stderr = invoke(str(executable), ["run"], args.timeout_seconds)
        free_after = shutil.disk_usage(disk_path).free
    ended = datetime.now().astimezone().isoformat(timespec="seconds")
    page = page_from(str(executable), args.timeout_seconds)
    current_run = envelope.get("data", {}).get("run_id", "") if isinstance(envelope.get("data"), dict) else ""
    if not current_run or page.get("run_id", "") != current_run:
        page = {}
        if args.run_json or code == 0:
            envelope = {
                "status": "failed",
                "code": "RESULT_PAGE_MISMATCH",
                "message": "本轮没有匹配同一 run_id 的自动结果页；已拒绝混入上一轮结果",
                "data": envelope.get("data", {}) if isinstance(envelope.get("data"), dict) else {},
                "next_steps": ["检查本轮原始输出和 result page 写入错误后再运行"],
            }
            code = 7
    if args.run_json:
        start_ns = int(page.get("window_start_ns", 0) or 0)
        end_ns = int(page.get("window_end_ns", 0) or 0)
        if start_ns:
            started = datetime.fromtimestamp(start_ns / 1_000_000_000.0).astimezone().isoformat(timespec="seconds")
        if end_ns:
            ended = datetime.fromtimestamp(end_ns / 1_000_000_000.0).astimezone().isoformat(timespec="seconds")
    entries = collect_entries(str(executable), page, args.timeout_seconds)
    # Rendering is capped at 200 lines, and the receipt states how many exist.
    # A page that silently showed the first 200 and said nothing reads as "these
    # are all of them".
    entries = entries[:MAX_LISTED_ENTRIES]
    receipt = build_receipt(code, envelope, page, entries, started, ended, str(disk_path), free_before, free_after, stderr)
    markdown = render(receipt)
    machine = json.dumps(receipt, ensure_ascii=False, indent=2) + "\n"
    atomic_write(output_dir / "latest-readable.md", markdown)
    atomic_write(output_dir / "latest-readable.json", machine)
    run_id = receipt["run_id"] or datetime.now().strftime("%Y%m%d-%H%M%S")
    safe_id = "".join(c if c.isalnum() or c in "-_" else "_" for c in run_id)[:100]
    atomic_write(output_dir / f"result-{safe_id}-readable.md", markdown)
    atomic_write(output_dir / f"result-{safe_id}-readable.json", machine)
    prune_snapshots(output_dir, 10)
    print(output_dir / "latest-readable.md")
    return code


if __name__ == "__main__":
    sys.exit(main())
