#!/usr/bin/env python3
"""Collect Dustkeeper's standard diagnostic artefacts into one read-only ZIP.

This is a diagnostic snapshot, not a consistent recovery backup and not a
provider ledger. A consistency backup needs the configuration, the state
database and its sidecars, every scope's complete .dustkeeper/ tree, and the old
binary with its version and digest. None of those are in the whitelist here.

The script only reads the four standard JSONL log files and reports/latest.json
under one state directory. It never runs dustkeeper, never contacts a model,
never uploads anything, and never modifies a source file. A custom result
directory is deliberately outside the whitelist: keep that page separately and
note its source and run id instead of growing a generic directory-discovery
system here.
"""

from __future__ import annotations

import argparse
import datetime
import errno
import hashlib
import json
import os
import stat
import sys
import zipfile
from pathlib import Path

SCHEMA_VERSION = 1
MANIFEST_NAME = "collection-manifest.json"

# One live log plus the three rotated files the bounded logger keeps.
LOG_NAMES = (
    "dustkeeper.jsonl",
    "dustkeeper.jsonl.1",
    "dustkeeper.jsonl.2",
    "dustkeeper.jsonl.3",
)
MAX_LOG_BYTES = (2 << 20) + (64 << 10)  # 2 MiB + 64 KiB
MAX_PAGE_BYTES = 4 << 20  # 4 MiB

# (archive name, relative source path, cap, required). Rotated logs are
# expected to be absent on a fresh installation, so absence there is a fact,
# not an incomplete collection.
SOURCES = tuple(
    ("logs/" + name, "logs/" + name, MAX_LOG_BYTES, name == LOG_NAMES[0])
    for name in LOG_NAMES
) + (("reports/latest.json", "reports/latest.json", MAX_PAGE_BYTES, True),)

TOTAL_CAP_BYTES = sum(cap for _, _, cap, _ in SOURCES)

O_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)


class CollectionError(Exception):
    """An argument or an output target made a trustworthy collection impossible."""


def snapshot(st: os.stat_result) -> dict:
    """The facts that let a reader notice a file changed while being read."""
    return {
        "size": st.st_size,
        "mtime_ns": st.st_mtime_ns,
        "dev": st.st_dev,
        "ino": st.st_ino,
        "mode": stat.S_IMODE(st.st_mode),
    }


def read_bounded(path: Path, cap: int) -> tuple[dict, bytes | None]:
    """Return one source's status and, when readable, the bytes to archive.

    Statuses are deliberately explicit so a missing, refused, unreadable,
    oversized or changing input is never silently presented as complete data.
    The second element is None exactly when nothing was included.
    """
    result = {
        "status": "missing",
        "reason": "",
        "size": None,
        "bytes_included": 0,
        "sha256": None,
        "before": None,
        "after": None,
        "changed_during_read": False,
    }

    def stop(status: str, reason: str = "") -> tuple[dict, None]:
        result["status"] = status
        result["reason"] = reason
        return result, None

    # Refuse a symlink anywhere on the whitelisted path rather than following it
    # out of the state directory. The paths themselves are fixed constants.
    state_dir = path.parent.parent if path.parent.name in ("logs", "reports") else path.parent
    for candidate in (state_dir, path.parent):
        try:
            if os.path.islink(str(candidate)):
                return stop("refused", "a symlink on the whitelisted path: " + str(candidate))
        except OSError as exc:
            return stop("unreadable", "cannot inspect path: " + os_error(exc))

    try:
        before = os.lstat(path)
    except FileNotFoundError:
        return stop("missing", "not present")
    except OSError as exc:
        return stop("unreadable", os_error(exc))

    result["before"] = snapshot(before)
    result["size"] = before.st_size
    if stat.S_ISLNK(before.st_mode):
        return stop("refused", "symlink at a whitelisted source; not followed")
    if not stat.S_ISREG(before.st_mode):
        return stop("refused", "not a regular file")
    if before.st_size > cap:
        return stop("oversize", "larger than the %d byte cap; not read" % cap)

    try:
        fd = os.open(path, os.O_RDONLY | O_NOFOLLOW)
    except OSError as exc:
        status = "refused" if exc.errno in (errno.ELOOP, errno.EPERM, errno.EACCES) else "unreadable"
        return stop(status, os_error(exc))

    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode):
            return stop("refused", "the opened file is not a regular file")
        if opened.st_size > cap:
            return stop("oversize", "grew past the %d byte cap before it could be read" % cap)
        # cap + 1 means growth during the read is visible instead of quietly
        # producing a truncated file that looks complete.
        data = b""
        while len(data) <= cap:
            chunk = os.read(fd, min(1 << 20, cap + 1 - len(data)))
            if not chunk:
                break
            data += chunk
        if len(data) > cap:
            return stop("oversize", "grew past the %d byte cap while being read" % cap)
        closed = os.fstat(fd)
    except OSError as exc:
        return stop("unreadable", os_error(exc))
    finally:
        os.close(fd)

    result["after"] = snapshot(closed)
    result["status"] = "ok"
    result["bytes_included"] = len(data)
    result["sha256"] = hashlib.sha256(data).hexdigest()
    # A live process may legitimately keep appending. The bytes are still
    # useful; the manifest says the archive is not one instant in time.
    if snapshot(closed) != snapshot(before):
        result["changed_during_read"] = True
        result["status"] = "changed"
        result["reason"] = "the file changed while it was being read; the included bytes are one bounded read"
    return result, data


def os_error(exc: OSError) -> str:
    return "%s (errno %s)" % (exc.strerror or str(exc), exc.errno)


def collect(state_dir: Path) -> tuple[dict, list[tuple[str, bytes]]]:
    entries: list[dict] = []
    payloads: list[tuple[str, bytes]] = []
    for arcname, rel, cap, required in SOURCES:
        info, data = read_bounded(state_dir / rel, cap)
        if data is not None:
            payloads.append((arcname, data))
        info.update({
            "name": arcname,
            "source": rel,
            "cap_bytes": cap,
            "required": required,
        })
        entries.append(info)

    counts: dict[str, int] = {}
    for info in entries:
        counts[info["status"]] = counts.get(info["status"], 0) + 1
    gaps = [
        "%s: %s%s" % (e["name"], e["status"], " (" + e["reason"] + ")" if e["reason"] else "")
        for e in entries
        if e["status"] not in ("ok", "missing") or (e["required"] and e["status"] == "missing")
    ]
    for e in entries:
        if e["status"] == "missing" and not e["required"]:
            gaps.append("%s: missing (optional rotated log)" % e["name"])

    complete = not gaps
    manifest = {
        "schema_version": SCHEMA_VERSION,
        "purpose": "diagnostic evidence only; NOT a consistent recovery backup and NOT a provider ledger",
        "script": "collect-run-data.py",
        "collected_at": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
        "state_dir": str(state_dir),
        "complete": complete,
        "complete_means": "every whitelisted source was readable, within its cap and unchanged; a false value never means the included bytes are wrong",
        "source_total_cap_bytes": TOTAL_CAP_BYTES,
        "included_bytes": sum(len(data) for _, data in payloads),
        "counts": counts,
        "files": entries,
        "gaps": gaps,
        "not_collected": [
            "config.json (may hold credentials)",
            "state.db and its WAL/SHM sidecars",
            "every scope's .dustkeeper/ tree",
            "the installed binary, its version and its SHA-256",
            "custom result-dir pages outside the standard reports/ location",
            "provider-side billing records",
        ],
        "notes": [
            "A missing optional rotated log is normal on a fresh installation; it is still listed as a gap so the archive does not claim full history.",
            "If dustkeeper was running during collection, the manifest records observable changes but the archive is not one instant in time.",
            "A result page may contain business file names and paths; share this ZIP accordingly.",
        ],
    }
    return manifest, payloads


def write_zip(output: Path, manifest: dict, payloads: list[tuple[str, bytes]]) -> None:
    parent = output.parent
    if not parent.is_dir():
        raise CollectionError("output parent directory does not exist: " + str(parent))
    # O_EXCL is the whole "do not overwrite" rule; 0600 keeps the evidence
    # private. Nothing is written to the source tree.
    try:
        fd = os.open(str(output), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError as exc:
        raise CollectionError("output already exists; choose a new name, never overwrite: " + str(output)) from exc
    except OSError as exc:
        raise CollectionError("cannot create output: " + os_error(exc)) from exc

    try:
        with os.fdopen(fd, "wb") as raw:
            with zipfile.ZipFile(raw, "w", compression=zipfile.ZIP_DEFLATED) as zf:
                for arcname, data in payloads:
                    info = zipfile.ZipInfo(arcname)
                    info.date_time = datetime.datetime.now().timetuple()[:6]
                    info.compress_type = zipfile.ZIP_DEFLATED
                    info.external_attr = 0o600 << 16
                    zf.writestr(info, data)
                body = json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True).encode("utf-8")
                info = zipfile.ZipInfo(MANIFEST_NAME)
                info.date_time = datetime.datetime.now().timetuple()[:6]
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = 0o600 << 16
                zf.writestr(info, body)
    except Exception:
        # A partial ZIP is worse than no ZIP: it looks like evidence but has no
        # manifest. Remove only the file this invocation just created.
        try:
            output.unlink()
        except OSError:
            pass
        raise


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Collect the standard Dustkeeper diagnostic artefacts into a new private ZIP. "
                    "This ZIP is for analysis, not a recovery backup.",
    )
    parser.add_argument("--state-dir", required=True, help="absolute state directory of one instance")
    parser.add_argument("--output", required=True, help="absolute path of a NEW .zip file")
    args = parser.parse_args(argv)

    state_dir = Path(args.state_dir)
    output = Path(args.output)
    try:
        if not state_dir.is_absolute() or os.path.abspath(str(state_dir)) != str(state_dir):
            raise CollectionError("--state-dir must be an absolute normalized path")
        if not output.is_absolute():
            raise CollectionError("--output must be an absolute path")
        if not state_dir.is_dir():
            raise CollectionError("state directory is not a directory: " + str(state_dir))
        manifest, payloads = collect(state_dir)
        write_zip(output, manifest, payloads)
    except CollectionError as exc:
        print("collect-run-data: " + str(exc), file=sys.stderr)
        return 2

    print("wrote " + str(output))
    print("  mode 0600, %d source file(s), %d byte(s) included" %
          (len(payloads), manifest["included_bytes"]))
    print("  complete: %s" % ("yes" if manifest["complete"] else "no"))
    for gap in manifest["gaps"]:
        print("  gap: " + gap)
    print("  diagnostic evidence only: it is not a consistent recovery backup")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
