"""Read-only directory measurements with a bounded, external 30-day audit record."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import sys
import tempfile


def category(relative):
    parts = relative.parts
    if relative.name == "result.tar.gz":
        return "result_archives"
    for name in ("utilization", "tasks", "service-logs", "agent", "offline", "maintenance-backups", "bin"):
        if name in parts:
            return name
    return "other"


def measure(root):
    totals = {"allocatedBytes": 0, "logicalBytes": 0, "files": 0, "directories": 0, "categories": {}}
    for folder, directories, files in os.walk(root, followlinks=False, onerror=lambda error: (_ for _ in ()).throw(error)):
        for name in directories + files:
            path = Path(folder) / name
            try:
                info = path.lstat()
            except FileNotFoundError:
                # An atomic replacement scratch file can disappear during a scan.
                # Retrying the whole scan is handled by the caller.
                if name.startswith((".watch-", ".archive-")):
                    continue
                raise
            is_directory = path.is_dir() and not path.is_symlink()
            allocated = getattr(info, "st_blocks", (info.st_size + 511) // 512) * 512
            logical = 0 if is_directory else info.st_size
            key = category(path.relative_to(root))
            group = totals["categories"].setdefault(key, {"allocatedBytes": 0, "logicalBytes": 0, "files": 0})
            group["allocatedBytes"] += allocated
            group["logicalBytes"] += logical
            group["files"] += int(not is_directory)
            totals["allocatedBytes"] += allocated
            totals["logicalBytes"] += logical
            totals["directories" if is_directory else "files"] += 1
    return totals


def evaluate(saved, sample, now):
    start = dt.datetime.fromisoformat(saved["startedAt"])
    elapsed = (now - start).total_seconds() / 86400
    growth = sample["allocatedBytes"] - saved["baseline"]["allocatedBytes"]
    categories = {}
    for key in set(saved["baseline"]["categories"]) | set(sample["categories"]):
        before = saved["baseline"]["categories"].get(key, {}).get("allocatedBytes", 0)
        after = sample["categories"].get(key, {}).get("allocatedBytes", 0)
        categories[key] = after - before
    history = saved.get("samples", [])
    peak = max([growth, saved.get("latest", {}).get("peakObservedGrowthBytes", growth)] +
               [row.get("peakObservedGrowthBytes", row["growthBytes"]) for row in history])
    limit = saved["limitBytes"]
    observed = set(saved.get("observedDates", []))
    if 0 <= elapsed <= saved["days"] + 1:
        observed.add(now.date().isoformat())
    dates = sorted(dt.date.fromisoformat(day) for day in observed)
    largest_gap = max([(right - left).days for left, right in zip(dates, dates[1:])] or [0])
    covered = len(dates) >= 29 and largest_gap <= 2
    status = "exceeded" if peak > limit else "within_target" if elapsed >= saved["days"] and covered else "insufficient_coverage" if elapsed >= saved["days"] else "in_progress"
    return {"status": status, "elapsedDays": round(elapsed, 4), "growthBytes": growth,
            "peakObservedGrowthBytes": peak, "limitBytes": limit, "categoryGrowthBytes": categories,
            "warning": growth >= limit * 0.8, "observedDays": len(dates), "largestSamplingGapDays": largest_gap,
            "scope": "observed retained directory allocation; not a continuous peak measurement"}


def write_state(path, saved, initial=False):
    encoded = (json.dumps(saved, ensure_ascii=False, indent=2) + "\n").encode()
    if len(encoded) > 128 * 1024:
        raise ValueError("audit state exceeded 128 KiB")
    path.parent.mkdir(parents=True, exist_ok=True)
    if initial:
        with path.open("xb") as stream:
            os.chmod(path, 0o600)
            stream.write(encoded)
        return
    fd, scratch = tempfile.mkstemp(prefix=".storage-audit-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(scratch, path)
    finally:
        if os.path.exists(scratch):
            os.unlink(scratch)


def run(root, state, initial=False, now=None):
    now = now or dt.datetime.now(dt.timezone.utc)
    root, state = Path(root).resolve(), Path(state).resolve()
    if not root.is_dir() or state == root or root in state.parents:
        raise ValueError("root must exist; audit state must be outside the measured directory")
    sample = measure(root)
    if initial:
        saved = {"schema": "simplehtmlwatch.storage-audit.v1", "root": str(root),
                 "startedAt": now.isoformat(), "days": 30, "limitBytes": 5_000_000,
                 "baseline": sample, "samples": [], "observedDates": []}
    else:
        saved = json.loads(state.read_text())
        if saved.get("schema") != "simplehtmlwatch.storage-audit.v1" or saved.get("root") != str(root) or saved.get("days") != 30 or saved.get("limitBytes") != 5_000_000:
            raise ValueError("audit identity or budget changed; original baseline was not reset")
    result = evaluate(saved, sample, now)
    row = {"at": now.isoformat(), "growthBytes": result["growthBytes"],
           "peakObservedGrowthBytes": result["peakObservedGrowthBytes"],
           "allocatedBytes": sample["allocatedBytes"], "categories": sample["categories"]}
    # Keep one snapshot per UTC date and at most 35 dates. Repeated checks cannot
    # grow the report without bound; the original baseline is never replaced.
    date = now.date().isoformat()
    rows = [r for r in saved["samples"] if r["at"][:10] != date]
    saved["samples"] = (rows + [row])[-35:]
    elapsed = (now - dt.datetime.fromisoformat(saved["startedAt"])).total_seconds() / 86400
    if 0 <= elapsed <= saved["days"] + 1:
        saved["observedDates"] = sorted(set(saved.get("observedDates", [])) | {date})[:32]
    result["sampleCount"] = len(saved["samples"])
    saved["latest"] = result
    write_state(state, saved, initial)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True)
    parser.add_argument("--state", required=True)
    parser.add_argument("--init", action="store_true", help="create baseline once; refuse to overwrite")
    args = parser.parse_args()
    result = run(args.root, args.state, args.init)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 3 if result["status"] == "exceeded" else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError) as error:
        print(f"storage audit: {error}", file=sys.stderr)
        sys.exit(2)
