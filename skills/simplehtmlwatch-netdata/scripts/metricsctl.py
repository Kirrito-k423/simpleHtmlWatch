#!/usr/bin/env python3
"""通过已核验的本机中台查询真实集群系统指标。"""
import argparse
import datetime
import json
import math
import os
import re
import sys
import time
import urllib.parse

from service_client import Client


def machines(client):
    config = client.api("/api/config")["config"]
    states = client.api("/api/status")
    if isinstance(states, list):
        states = {s["machineId"]: s for s in states}
    result = []
    for m in config["machines"]:
        state = states.get(m["id"], {})
        agent = m.get("netdata") or {}
        row = {k: m.get(k) for k in ("id", "name", "host", "port", "group", "enabled")}
        row["netdata"] = {"enabled": bool(agent.get("enabled")), "port": agent.get("port") or 19999}
        row["ssh"] = {k: state[k] for k in ("status", "error", "updatedAt", "lastSuccess", "keyChanged") if k in state}
        result.append(row)
    return result


def query_path(machine=None, task=None, after=None, before=None):
    if bool(machine) == bool(task):
        raise ValueError("须指定一个机器 ID 或任务 ID")
    selector = machine or task
    if not re.fullmatch(r"[a-zA-Z0-9_-]{1,80}", selector):
        raise ValueError("机器或任务 ID 格式无效")
    params = {"machineId" if machine else "taskId": selector}
    if (after is None) != (before is None):
        raise ValueError("起止时间须同时提供")
    if after is not None:
        if after < 1 or before <= after or before - after > 7 * 86400 or before > int(time.time()) + 60:
            raise ValueError("查询范围无效：最多 7 天，结束不能晚于当前时间")
        params.update(after=after, before=before)
    return "/api/metrics?" + urllib.parse.urlencode(params)


def summarize(snapshot):
    charts = snapshot.get("charts")
    if not isinstance(charts, list) or len(charts) != 4:
        raise ValueError("指标响应须包含四项图表")
    result = {k: snapshot[k] for k in ("machineId", "machineName", "source", "agentVersion", "nodeId", "agentTime", "clockSkewSeconds", "fetchedAt", "after", "before", "windowClipped", "task") if k in snapshot}
    result["charts"] = []
    partial = False
    for chart in charts:
        row = {k: chart[k] for k in ("context", "name", "unit", "updateEvery", "step", "lastEntry", "after", "before", "error") if k in chart}
        row["series"] = []
        count = 0
        for series in chart.get("series") or []:
            points = series.get("points") or []
            valid = [p for p in points if isinstance(p.get("value"), (int, float)) and not isinstance(p["value"], bool) and math.isfinite(p["value"])]
            count += len(valid)
            row["series"].append({"name": series["name"], "unit": series.get("unit", chart.get("unit")), "valuePoints": len(valid), "missingPoints": len(points) - len(valid), "lastInWindow": max(valid, key=lambda p: p["at"]) if valid else None})
        row["valuePoints"] = count
        if row.get("error") or count == 0:
            partial = True
        result["charts"].append(row)
    result["complete"] = not partial
    return result, partial


def utilization_path(args):
    params = {}
    if args.date:
        if args.period:
            raise ValueError("date 与 period 不能同时提供")
        try:
            parsed = datetime.date.fromisoformat(args.date)
        except ValueError as exc:
            raise ValueError("日期格式须为 YYYY-MM-DD") from exc
        if parsed.isoformat() != args.date:
            raise ValueError("日期格式须为 YYYY-MM-DD")
        params.update(date=args.date, days=args.days or 1)
    else:
        if args.days is not None:
            raise ValueError("days 须与 date 一起提供")
        params["period"] = args.period or "yesterday"
    if args.days is not None and not 1 <= args.days <= 30:
        raise ValueError("查询天数须为 1–30")
    if args.machine:
        if not re.fullmatch(r"[a-zA-Z0-9_-]{1,80}", args.machine):
            raise ValueError("机器 ID 格式无效")
        params["machineId"] = args.machine
    if args.group:
        params["group"] = args.group
    return "/api/utilization?" + urllib.parse.urlencode(params)


def run(args, client):
    if args.action == "discover":
        return client.discover(), 0
    if args.action == "machines":
        return {"machines": machines(client)}, 0
    if args.action == "utilization":
        output = client.api(utilization_path(args))
        if not isinstance(output.get("machines"), list) or output.get("source") != "npu-smi":
            raise ValueError("利用率汇总响应无效")
        # Coverage is a separate measurement; absent history is never a zero.
        output["utilizationComplete"] = bool(output["machines"]) and not output.get("storageError") and all(
            row.get("mean") is not None and row.get("coveragePercent", 0) >= 90
            for row in output["machines"]
        )
        output["occupancyComplete"] = bool(output["machines"]) and not output.get("storageError") and all(
            row.get("occupiedPercent") is not None and row.get("occupancyCoveragePercent", 0) >= 90
            for row in output["machines"]
        )
        output["complete"] = output["utilizationComplete"] and output["occupancyComplete"]
        return output, 0 if output["complete"] else 3
    if args.action == "query":
        raw = client.api(query_path(args.machine, args.task, args.after, args.before))
        summary, partial = summarize(raw)
        return raw if args.raw else summary, 3 if partial else 0
    rows = machines(client)
    results, skipped = [], []
    for m in rows:
        if args.group and m["group"] != args.group:
            continue
        if not m["enabled"] or not m["netdata"]["enabled"]:
            skipped.append({"id": m["id"], "name": m["name"], "reason": "机器已停用或尚未启用 Netdata", "ssh": m["ssh"]})
            continue
        try:
            raw = client.api(query_path(machine=m["id"], after=args.after, before=args.before))
            summary, partial = summarize(raw)
            results.append({"id": m["id"], "ok": not partial, "metrics": raw if args.raw else summary})
        except (OSError, ValueError, RuntimeError, KeyError, TypeError) as exc:
            results.append({"id": m["id"], "ok": False, "error": str(exc)})
    failed = sum(not r["ok"] for r in results)
    out = {"queried": len(results), "succeeded": len(results) - failed, "failed": failed, "skipped": skipped, "results": results}
    return out, 3 if failed or skipped or not results else 0


def main(argv=None):
    p = argparse.ArgumentParser(description="只读发现 simpleHtmlWatch 实例并查询 Netdata 集群指标")
    p.add_argument("--instance", default=os.environ.get("SHW_INSTANCE", "default"), help="稳定实例名，默认 default")
    p.add_argument("--data-dir", default=os.environ.get("SHW_DATA_DIR"), help="可选实例数据目录")
    p.add_argument("--url", default=os.environ.get("SHW_URL"), help="可选本机实际 URL，不回退其他实例")
    p.add_argument("--service-id", help="要求稳定 serviceId")
    p.add_argument("--expect-version", help="要求精确版本，不匹配时报错")
    p.add_argument("--require-capability", action="append", default=[], help="附加能力要求，可重复")
    sub = p.add_subparsers(dest="action", required=True)
    sub.add_parser("discover", help="发现并核验在线实例身份")
    sub.add_parser("machines", help="列出注册机器、SSH 状态及接入开关")
    q = sub.add_parser("query", help="查询一台机器或一个任务的系统指标")
    selector = q.add_mutually_exclusive_group(required=True)
    selector.add_argument("--machine", help="机器 ID")
    selector.add_argument("--task", help="任务 ID")
    s = sub.add_parser("summary", help="串行查询目标组所有已启用机器，报告跳过及失败")
    s.add_argument("--group", help="限制机器组")
    u = sub.add_parser("utilization", help="查询机器级 NPU 小时汇总，不需要启用 Netdata")
    u.add_argument("--period", choices=("today", "yesterday", "week"), help="今天、昨天或近 7 个完整自然日，默认昨天")
    u.add_argument("--date", help="起始北京时间日期 YYYY-MM-DD")
    u.add_argument("--days", type=int, help="与 date 一起提供，最多 30 个自然日")
    u.add_argument("--machine", help="限制机器 ID")
    u.add_argument("--group", help="限制机器组")
    for child in (q, s):
        child.add_argument("--after", type=int, help="起始 Unix 秒，与 before 成对")
        child.add_argument("--before", type=int, help="结束 Unix 秒，最多 7 天")
        child.add_argument("--raw", action="store_true", help="输出完整曲线与任务标记")
    args = p.parse_args(argv)
    try:
        if args.action in ("query", "summary"):
            query_path(machine="validation", after=args.after, before=args.before)
        if args.action == "utilization":
            utilization_path(args)
        output, code = run(args, Client(args))
        print(json.dumps(output, ensure_ascii=False, indent=2, allow_nan=False))
        return code
    except (OSError, ValueError, RuntimeError, KeyError, TypeError) as exc:
        print(json.dumps({"error": str(exc)}, ensure_ascii=False), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
