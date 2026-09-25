#!/usr/bin/env python3
"""通过本机 simpleHtmlWatch API 管理远端任务。"""

import argparse
import html
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        raise ValueError("不允许 HTTP 重定向")


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def base_url(raw):
    parts = urllib.parse.urlsplit(raw)
    if parts.scheme != "http" or parts.hostname not in ("127.0.0.1", "localhost"):
        raise ValueError("仅允许本机 HTTP 地址")
    if parts.username or parts.password or parts.path not in ("", "/") or parts.query or parts.fragment:
        raise ValueError("地址只能包含本机主机名和端口")
    if not parts.port or not 1 <= parts.port <= 65535:
        raise ValueError("请提供中台实际监听端口")
    return f"http://127.0.0.1:{parts.port}"


def request(url, token=None, method="GET", data=None, binary=False):
    headers = {"X-Watch-Token": token} if token else {}
    if data is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(data, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        response = OPENER.open(req, timeout=45)
        if binary:
            return response
        with response:
            return response.read(1024 * 1024).decode("utf-8")
    except urllib.error.HTTPError as exc:
        detail = exc.read(4096).decode("utf-8", "replace")
        try:
            detail = json.loads(detail).get("error", detail)
        except json.JSONDecodeError:
            pass
        raise RuntimeError(f"HTTP {exc.code}：{detail}") from exc


def session(url):
    page = request(url + "/")
    match = re.search(r'<meta\s+name="watch-token"\s+content="([^"]+)"', page)
    if not match:
        raise RuntimeError("首页没有会话令牌，请确认连接的是 simpleHtmlWatch")
    return html.unescape(match.group(1))


def api(url, token, path, method="GET", data=None):
    raw = request(url + "/api/tasks" + path, token, method, data)
    return json.loads(raw)


def show(value):
    print(json.dumps(value, ensure_ascii=False, indent=2))


def main():
    parser = argparse.ArgumentParser(description="通过本机 SSH 任务中台提交、跟踪与回收任务")
    parser.add_argument("--url", default=os.environ.get("SHW_URL"), help="中台本机地址，例如 http://127.0.0.1:8765；也可设置 SHW_URL")
    sub = parser.add_subparsers(dest="action", required=True)
    ready = sub.add_parser("ready", help="列出可调度机器")
    ready.add_argument("--group")
    ready.add_argument("--machine")
    submit = sub.add_parser("submit", help="提交一条长任务")
    selector = submit.add_mutually_exclusive_group()
    selector.add_argument("--group")
    selector.add_argument("--machine")
    command = submit.add_mutually_exclusive_group(required=True)
    command.add_argument("--command")
    command.add_argument("--command-file")
    submit.add_argument("--id", help="重发相同请求时复用原任务 ID")
    for action in ("status", "logs", "collect", "resolve", "download", "wait"):
        p = sub.add_parser(action, help={"status": "查看任务状态", "logs": "读取日志末尾", "collect": "重试回收结果", "resolve": "人工确认后解除未知任务占用", "download": "下载结果包", "wait": "轮询直到完成"}[action])
        p.add_argument("id")
        if action == "resolve":
            p.add_argument("--confirm-remote-stopped", action="store_true", help="确认已独立核实远端进程停止")
        if action == "download":
            p.add_argument("--out", required=True)
        if action == "wait":
            p.add_argument("--interval", type=float, default=3)
    args = parser.parse_args()
    if not args.url:
        raise ValueError("请提供 --url 或 SHW_URL，使用程序控制台显示的监听地址")
    url = base_url(args.url)
    token = session(url)
    if args.action == "ready":
        if args.group and args.machine:
            raise ValueError("ready 只能指定一个选择器")
        query = urllib.parse.urlencode({"group": args.group or "", "machineId": args.machine or ""})
        show(api(url, token, "/ready?" + query))
    elif args.action == "submit":
        if args.command_file:
            with open(args.command_file, encoding="utf-8") as source:
                shell = source.read()
        else:
            shell = args.command
        task_id = args.id or uuid.uuid4().hex
        payload = {"id": task_id, "shell": shell}
        if args.machine:
            payload["machineId"] = args.machine
        elif args.group:
            payload["group"] = args.group
        print(f"任务 ID：{task_id}", flush=True)
        show(api(url, token, "", "POST", payload))
    elif args.action == "status":
        show(api(url, token, "?" + urllib.parse.urlencode({"id": args.id})))
    elif args.action == "logs":
        show(api(url, token, "/logs?" + urllib.parse.urlencode({"id": args.id})))
    elif args.action == "collect":
        show(api(url, token, "/collect?" + urllib.parse.urlencode({"id": args.id}), "POST"))
    elif args.action == "resolve":
        if not args.confirm_remote_stopped:
            raise ValueError("需先独立核实远端进程已停止，再加 --confirm-remote-stopped")
        show(api(url, token, "/resolve", "POST", {"id": args.id, "confirm": "remote-stopped"}))
    elif args.action == "wait":
        if args.interval < 1:
            raise ValueError("轮询间隔至少 1 秒")
        last = None
        while True:
            job = api(url, token, "?" + urllib.parse.urlencode({"id": args.id}))
            summary = (job["status"], job.get("error"), job.get("archiveReady"), job.get("archiveError"))
            if summary != last:
                show(job)
                last = summary
            if job.get("finishedAt"):
                return 0 if job.get("status") == "succeeded" and job.get("archiveReady") else 1
            time.sleep(args.interval)
    elif args.action == "download":
        path = os.path.abspath(args.out)
        endpoint = url + "/api/tasks/archive?" + urllib.parse.urlencode({"id": args.id})
        response = request(endpoint, token, binary=True)
        total = 0
        created = False
        try:
            with response, open(path, "xb") as target:
                created = True
                while True:
                    chunk = response.read(65536)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > 5 * 1024 * 1024:
                        raise RuntimeError("结果包超过 5 MiB")
                    target.write(chunk)
        except Exception:
            if created:
                os.remove(path)
            raise
        print(f"已保存：{path}（{total} 字节）")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, OSError, json.JSONDecodeError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        sys.exit(2)
