#!/usr/bin/env python3
"""通过本机 simpleHtmlWatch API 管理远端任务。"""

import argparse
import html
import http.client
import json
import os
from pathlib import Path
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
MAX_TASK_ARCHIVE = 10 * 1024 * 1024 * 1024
ARCHIVE_IDLE_TIMEOUT = 5 * 60


def base_url(raw):
    parts = urllib.parse.urlsplit(raw)
    if parts.scheme != "http" or parts.hostname not in ("127.0.0.1", "localhost"):
        raise ValueError("仅允许本机 HTTP 地址")
    if parts.username or parts.password or parts.path not in ("", "/") or parts.query or parts.fragment:
        raise ValueError("地址只能包含本机主机名和端口")
    if not parts.port or not 1 <= parts.port <= 65535:
        raise ValueError("请提供中台实际监听端口")
    return f"http://127.0.0.1:{parts.port}"


class HTTPFailure(RuntimeError):
    def __init__(self, status, detail):
        self.status = status
        super().__init__(f"HTTP {status}：{detail}")


def request(url, token=None, method="GET", data=None, binary=False, run_id=None, timeout=45):
    headers = {"X-Watch-Token": token} if token else {}
    if run_id:
        headers["X-Watch-Instance"] = run_id
    if data is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(data, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        response = OPENER.open(req, timeout=timeout)
        if binary:
            return response
        with response:
            return response.read(1024 * 1024).decode("utf-8")
    except urllib.error.HTTPError as exc:
        with exc:
            detail = exc.read(4096).decode("utf-8", "replace")
        try:
            detail = json.loads(detail).get("error", detail)
        except json.JSONDecodeError:
            pass
        raise HTTPFailure(exc.code, detail) from exc


def session(url, run_id=None):
    page = request(url + "/", run_id=run_id)
    match = re.search(r'<meta\s+name="watch-token"\s+content="([^"]+)"', page)
    if not match:
        raise RuntimeError("首页没有会话令牌，请确认连接的是 simpleHtmlWatch")
    return html.unescape(match.group(1))


SERVICE_SCHEMA = "simplehtmlwatch.service.v1"
NO_RESTART = "禁止因探测失败自动重启、换版本、删除锁文件或按进程名结束服务"


def service_dir(raw, instance):
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}", instance):
        raise ValueError("实例名需为 1–64 位字母、数字、下划线或连字符，首字符必须为字母或数字")
    if raw:
        return Path(raw).expanduser().resolve()
    if sys.platform == "darwin":
        root = Path.home() / "Library" / "Application Support"
    elif os.name == "nt":
        if not os.environ.get("APPDATA"):
            raise ValueError("无法定位 APPDATA；请显式指定 --data-dir")
        root = Path(os.environ["APPDATA"])
    else:
        root = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    directory = root / "simpleHtmlWatch"
    if instance != "default":
        directory = directory / "instances" / instance
    return directory.resolve()


class Client:
    """Discover before each operation; never launch, stop or replay a request."""

    def __init__(self, args):
        self.url_override = base_url(args.url) if args.url else None
        self.directory = service_dir(args.data_dir, args.instance)
        self.instance = args.instance
        self.allow_legacy = args.allow_legacy
        self.expect_version = args.expect_version
        self.capabilities = set(args.require_capability)
        if args.action != "discover":
            self.capabilities.add("tasks")
        self.service_id = args.service_id
        self.run_id = None
        self.url = None
        self.token = None
        if self.url_override and args.data_dir:
            raise ValueError("--url 与 --data-dir 不能同时指定；选择一个权威发现来源")
        if self.allow_legacy and (not self.url_override or self.service_id or self.expect_version or args.require_capability):
            raise ValueError("--allow-legacy 仅用于显式 URL，不能与身份、版本或能力要求共用")

    def discover(self):
        expected = None
        if self.url_override:
            url = self.url_override
        else:
            path = self.directory / "service.json"
            try:
                expected = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise RuntimeError(f"无法读取实例登记 {path}：{exc}。旧版请核对实际地址；{NO_RESTART}") from exc
            self.validate(expected)
            if Path(expected.get("dataDir", "")).resolve() != self.directory:
                raise RuntimeError("登记文件属于其他数据目录；拒绝连接")
            url = base_url(expected["url"])
        try:
            raw = request(url + "/healthz", timeout=2)
        except HTTPFailure as exc:
            if self.allow_legacy and exc.status == 404:
                return {"legacy": True, "url": url, "instance": self.instance}
            raise RuntimeError(f"实例身份接口不可用：{exc}；{NO_RESTART}") from exc
        except OSError as exc:
            raise RuntimeError(f"实例不可达 {url}：{exc}；{NO_RESTART}。请按实例重新发现或交由服务维护者处理") from exc
        try:
            live = json.loads(raw)
        except ValueError as exc:
            raise RuntimeError(f"端口响应不是实例身份 JSON；{NO_RESTART}") from exc
        self.validate(live)
        if live["url"] != url:
            raise RuntimeError("端口上的实例声明了其他地址；拒绝连接")
        if expected:
            for key in ("serviceId", "runId", "instance", "buildId", "version", "dataDir", "url"):
                if live.get(key) != expected.get(key):
                    raise RuntimeError(f"端口上的服务与登记 {key} 不一致；{NO_RESTART}")
        if self.service_id and live["serviceId"] != self.service_id:
            raise RuntimeError("实例 serviceId 已变化；拒绝把原任务转到另一个中台")
        self.service_id = live["serviceId"]
        return live

    def validate(self, info):
        if not isinstance(info, dict) or info.get("schema") != SERVICE_SCHEMA or info.get("apiVersion") != 1:
            raise RuntimeError(f"服务身份或 API 协议不兼容；{NO_RESTART}")
        for key in ("serviceId", "runId", "buildId", "dataDir", "version", "url"):
            if not isinstance(info.get(key), str) or not info[key]:
                raise RuntimeError(f"服务身份缺少 {key}；拒绝连接")
        if info.get("instance") != self.instance:
            raise RuntimeError(f"发现实例 {info.get('instance')!r}，要求 {self.instance!r}；拒绝串用实例")
        if self.expect_version and info["version"] != self.expect_version:
            raise RuntimeError(f"服务版本 {info['version']} 不符合 {self.expect_version}；版本不符不等于服务宕机")
        capabilities = info.get("capabilities")
        if not isinstance(capabilities, list) or not all(isinstance(item, str) for item in capabilities):
            raise RuntimeError("服务能力列表无效；拒绝连接")
        missing = self.capabilities - set(capabilities)
        if missing:
            raise RuntimeError(f"服务缺少能力 {sorted(missing)}；不可据此替换版本或重启")

    def prepare(self):
        info = self.discover()
        url, run_id = info["url"], info.get("runId")
        if self.token is None or (url, run_id) != (self.url, self.run_id):
            self.token = session(url, run_id)
        self.url, self.run_id = url, run_id

    def api(self, path, method="GET", data=None, binary=False, timeout=45):
        self.prepare()
        raw = request(self.url + "/api/tasks" + path, self.token, method, data, binary, self.run_id, timeout)
        return raw if binary else json.loads(raw)


def save_archive(response, path):
    """分块保存结果包；拒绝覆盖，失败或中断时移除本次残留文件。"""
    total = 0
    created = False
    try:
        with response:
            length = response.headers.get("Content-Length")
            expected = int(length) if length is not None else None
            if expected is not None and (expected < 0 or expected > MAX_TASK_ARCHIVE):
                raise RuntimeError("结果包长度无效或超过 10 GiB 上限")
            with open(path, "xb") as target:
                created = True
                while True:
                    chunk = response.read(65536)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > MAX_TASK_ARCHIVE:
                        raise RuntimeError("结果包超过 10 GiB 上限")
                    target.write(chunk)
                if expected is not None and total != expected:
                    raise RuntimeError(f"结果包下载不完整：预期 {expected} 字节，收到 {total} 字节")
                if total == 0:
                    raise RuntimeError("结果包为空")
    except BaseException:
        if created:
            os.remove(path)
        raise
    return total


def show(value):
    print(json.dumps(value, ensure_ascii=False, indent=2))


def main():
    parser = argparse.ArgumentParser(description="通过本机 SSH 任务中台提交、跟踪与回收任务")
    parser.add_argument("--url", default=os.environ.get("SHW_URL"), help="可选：固定 URL（SHW_URL），不自动回退其他实例；默认按登记发现")
    parser.add_argument("--instance", default=os.environ.get("SHW_INSTANCE", "default"), help="稳定实例名（SHW_INSTANCE），默认 default")
    parser.add_argument("--data-dir", default=os.environ.get("SHW_DATA_DIR"), help="实例数据目录（SHW_DATA_DIR），可代替 URL")
    parser.add_argument("--service-id", help="要求稳定 serviceId，防止误连重建的数据目录")
    parser.add_argument("--expect-version", help="要求精确软件版本；不匹配时报错，不重启")
    parser.add_argument("--require-capability", action="append", default=[], help="要求能力，可重复；任务操作默认要求 tasks")
    parser.add_argument("--allow-legacy", action="store_true", help="仅显式 URL：允许没有身份接口的历史版本，不具备身份校验")
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("discover", help="只读发现并校验实例、版本、能力和当前地址")
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
    client = Client(args)
    if args.action == "discover":
        show(client.discover())
        return 0
    if args.action == "ready":
        if args.group and args.machine:
            raise ValueError("ready 只能指定一个选择器")
        query = urllib.parse.urlencode({"group": args.group or "", "machineId": args.machine or ""})
        show(client.api("/ready?" + query))
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
        show(client.api("", "POST", payload))
    elif args.action == "status":
        show(client.api("?" + urllib.parse.urlencode({"id": args.id})))
    elif args.action == "logs":
        show(client.api("/logs?" + urllib.parse.urlencode({"id": args.id})))
    elif args.action == "collect":
        # 服务端按 SSH 连续无进度 5 分钟中止；大包回收不设总时长限制。
        show(client.api("/collect?" + urllib.parse.urlencode({"id": args.id}), "POST", timeout=None))
    elif args.action == "resolve":
        if not args.confirm_remote_stopped:
            raise ValueError("需先独立核实远端进程已停止，再加 --confirm-remote-stopped")
        show(client.api("/resolve", "POST", {"id": args.id, "confirm": "remote-stopped"}))
    elif args.action == "wait":
        if args.interval < 1:
            raise ValueError("轮询间隔至少 1 秒")
        last = None
        while True:
            job = client.api("?" + urllib.parse.urlencode({"id": args.id}))
            summary = (job["status"], job.get("error"), job.get("archiveReady"), job.get("archiveError"))
            if summary != last:
                show(job)
                last = summary
            if job.get("finishedAt") and (job.get("archiveReady") or job.get("archiveError") or job.get("exitCode") is None):
                return 0 if job.get("status") == "succeeded" and job.get("archiveReady") else 1
            time.sleep(args.interval)
    elif args.action == "download":
        path = os.path.abspath(args.out)
        response = client.api("/archive?" + urllib.parse.urlencode({"id": args.id}), binary=True, timeout=ARCHIVE_IDLE_TIMEOUT)
        total = save_archive(response, path)
        print(f"已保存：{path}（{total} 字节）")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, OSError, http.client.HTTPException) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        sys.exit(2)
