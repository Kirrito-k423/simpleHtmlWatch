#!/usr/bin/env python3
"""通过本机 simpleHtmlWatch API 管理远端任务。"""

import argparse
import hashlib
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


def request(url, token=None, method="GET", data=None, binary=False, run_id=None, timeout=45, max_response_bytes=1024 * 1024):
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
            body = response.read(max_response_bytes + 1)
            if len(body) > max_response_bytes:
                raise RuntimeError("响应超过客户端上限，未使用截断数据")
            return body.decode("utf-8")
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

    def continuation_api(self, path="", method="GET", data=None):
        if self.allow_legacy:
            raise ValueError("实验等待协议不允许 legacy 模式")
        self.capabilities.add("experiment-continuations-v1")
        self.prepare()
        raw = request(self.url + "/api/continuations" + path, self.token, method,
                      data, run_id=self.run_id, max_response_bytes=16 * 1024 * 1024)
        return json.loads(raw)


def checkpoint_digest(checkpoint):
    if not isinstance(checkpoint, dict) or not isinstance(checkpoint.get("path"), str):
        raise ValueError("检查点需包含绝对路径 path")
    path = Path(checkpoint["path"])
    if not path.is_absolute() or path.stat().st_size > 1024 * 1024:
        raise ValueError("检查点需为不超过 1 MiB 的绝对路径文件")
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    expected = checkpoint.get("digest")
    if expected and digest != expected:
        raise ValueError("检查点已变化；先核对原实验，不自动执行旧计划")
    return digest


def client_arguments(parser):
    parser.add_argument("--url", default=os.environ.get("SHW_URL"), help="显式本机 URL；默认发现登记实例")
    parser.add_argument("--instance", default=os.environ.get("SHW_INSTANCE", "default"))
    parser.add_argument("--data-dir", default=os.environ.get("SHW_DATA_DIR"))
    parser.add_argument("--service-id", help="绑定稳定 serviceId")
    parser.add_argument("--expect-version")
    parser.add_argument("--require-capability", action="append", default=[])
    parser.add_argument("--allow-legacy", action="store_true")


def continuation_actions(sub):
    p = sub.add_parser("await-resources", help="保存已授权实验的资源等待合同")
    p.add_argument("--request", required=True, help="含固定等待 ID 和任务计划的 JSON 文件")
    p = sub.add_parser("continuation-status", help="查询等待单或全部等待单")
    p.add_argument("id", nargs="?")
    p = sub.add_parser("continuation-evaluate", help="原对话心跳立即评估资源")
    p.add_argument("id")
    p = sub.add_parser("continuation-control", help="暂停、取消或恢复后续工作，不杀远端任务")
    p.add_argument("id")
    p.add_argument("action_name", choices=("pause", "cancel", "resume", "set-mode"))
    p.add_argument("--wake-mode", choices=("bridge", "heartbeat"))
    for name in ("continuation-claim", "continuation-submit", "continuation-report"):
        p = sub.add_parser(name, help="按原对话、代数与检查点领取 / 提交 / 汇报实验")
        p.add_argument("id")
        p.add_argument("--thread", required=True)
        p.add_argument("--generation", type=int, required=True)
        if name == "continuation-submit":
            p.add_argument("--grant", required=True)
        else:
            p.add_argument("--event", required=True)
        if name == "continuation-report":
            p.add_argument("--action", dest="report_action", choices=("complete", "release-unused"), required=True)
            p.add_argument("--outcome", choices=("succeeded", "failed"))
            p.add_argument("--evidence", default="")


def run_continuation_action(client, args):
    if args.action == "await-resources":
        path = Path(args.request)
        if path.stat().st_size > 1024 * 1024:
            raise ValueError("等待合同不能超过 1 MiB")
        payload = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(payload, dict) or not payload.get("id") or not isinstance(payload.get("checkpoint"), dict):
            raise ValueError("等待合同需包含固定 id 与 checkpoint")
        payload["checkpoint"]["digest"] = checkpoint_digest(payload["checkpoint"])
        print(f"等待单 ID：{payload['id']}（传输不确定时先查询，不换 ID）", flush=True)
        saved = client.continuation_api("", "POST", payload)
        show(saved)
        if saved["policy"]["wakeMode"] == "heartbeat":
            print("等待单已保存；原对话仍需配置宿主 heartbeat。恢复时使用此版本客户端：" + str(Path(__file__).resolve()))
        else:
            print("等待单已保存；主动续聊仍需连接已核实原对话归属的 bridge。")
        return 0
    query = "?" + urllib.parse.urlencode({"id": args.id}) if args.id else ""
    if args.action == "continuation-status":
        show(client.continuation_api(query))
        return 0
    if args.action in ("continuation-control", "continuation-evaluate"):
        suffix = "/control" if args.action == "continuation-control" else "/evaluate"
        payload = {"id": args.id}
        if suffix == "/control":
            payload["action"] = args.action_name
            if args.action_name == "set-mode":
                if not args.wake_mode:
                    raise ValueError("set-mode 需提供 --wake-mode")
                payload["wakeMode"] = args.wake_mode
        show(client.continuation_api(suffix, "POST", payload))
        return 0
    job = client.continuation_api(query)
    if job["target"]["threadId"] != args.thread or job["generation"] != args.generation:
        raise ValueError("原对话或等待代数不匹配；拒绝接手")
    digest = checkpoint_digest(job["checkpoint"])
    if args.action == "continuation-submit":
        if job.get("grantId") != args.grant or job["state"] not in ("claimed", "executing"):
            raise ValueError("持有资源代数无效；先领取并查询原任务")
        plan = {task["id"]: task["shell"] for task in job["tasks"]}
        for allocation in job["allocations"]:
            payload = {"id": allocation["taskId"], "shell": plan[allocation["taskId"]],
                       "machineId": allocation["machineId"], "continuationId": job["id"],
                       "generation": job["generation"], "grantId": job["grantId"]}
            print(f"固定任务 ID：{payload['id']}", flush=True)
            # Query before submission, including when resuming a partial bundle.
            try:
                old = client.api("?" + urllib.parse.urlencode({"id": payload["id"]}))
            except HTTPFailure as exc:
                if exc.status != 404:
                    raise
            else:
                if any(old.get(key) != value for key, value in payload.items()):
                    raise ValueError("原任务请求不同；拒绝复用 ID")
                show(old)
                continue
            show(client.api("", "POST", payload))
        return 0
    if job.get("wake", {}).get("id") != args.event:
        raise ValueError("唤醒事件过期；查询当前等待单")
    payload = {"id": args.id, "generation": args.generation, "wakeEventId": args.event,
               "threadId": args.thread, "checkpointDigest": digest}
    suffix = "/claim"
    if args.action == "continuation-report":
        suffix = "/report"
        payload.update(action=args.report_action, outcome=args.outcome or "", evidence=args.evidence)
    show(client.continuation_api(suffix, "POST", payload))
    return 0


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
    continuation_actions(sub)
    cleanup_status = sub.add_parser("cleanup-status", help="只读查看常驻清理策略及诊断摘要")
    cleanup_status.add_argument("--id", help="读取一条完整诊断和清理日志")
    cleanup_policy = sub.add_parser("cleanup-policy", help="设置已授权的常驻失败残留清理策略")
    cleanup_switch = cleanup_policy.add_mutually_exclusive_group(required=True)
    cleanup_switch.add_argument("--enable", action="store_true")
    cleanup_switch.add_argument("--disable", action="store_true")
    cleanup_policy.add_argument("--authorization-ref", default="")
    sub.add_parser("discover", help="只读发现并校验实例、版本、能力和当前地址")
    ready = sub.add_parser("ready", help="列出可调度机器")
    ready.add_argument("--group")
    ready.add_argument("--machine")
    sub.add_parser("machines", help="列出机器入口、物理资源标识和占用原因")
    reservations = sub.add_parser("reservations", help="只读查询预约，写请求不确定时查询原 ID")
    reservations.add_argument("--id")
    reserve = sub.add_parser("reserve", help="按固定请求文件预约单机或原子整组，不启动远端任务")
    reserve.add_argument("--request", required=True)
    release = sub.add_parser("release-reservation", help="释放自己的预约；有未结束任务或不确定启动意图时拒绝")
    release.add_argument("id")
    submit = sub.add_parser("submit", help="提交一条长任务")
    selector = submit.add_mutually_exclusive_group()
    selector.add_argument("--group")
    selector.add_argument("--machine")
    command = submit.add_mutually_exclusive_group(required=True)
    command.add_argument("--command")
    command.add_argument("--command-file")
    submit.add_argument("--id", help="重发相同请求时复用原任务 ID")
    submit.add_argument("--reservation", help="已有预约 ID，须同时指定 --id 和 --machine")
    submit.add_argument("--reservation-grant", help="整组预约返回的固定 grantId")
    submit.add_argument("--reservation-generation", type=int, default=0)
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
    if args.action == "await-resources" or args.action.startswith("continuation-"):
        return run_continuation_action(client, args)
    if args.action in ("cleanup-status", "cleanup-policy"):
        if args.allow_legacy:
            raise ValueError("清理策略不允许 legacy 模式")
        client.capabilities.add("queue-residue-cleanup-v1")
        if args.action == "cleanup-policy":
            if args.enable and not args.authorization_ref.strip():
                raise ValueError("启用清理策略需提供已有用户授权的记录 --authorization-ref")
            show(client.api("/cleanup", "POST", {"enabled": args.enable, "authorizationRef": args.authorization_ref}))
        else:
            query = "?" + urllib.parse.urlencode({"id": args.id}) if args.id else ""
            show(client.api("/cleanup" + query))
        return 0
    if args.action == "discover":
        show(client.discover())
        return 0
    if args.action in ("reserve", "reservations", "release-reservation"):
        if args.allow_legacy:
            raise ValueError("预约客户端不允许 legacy 模式")
        client.capabilities.add("reservations")
        if args.action == "reserve":
            path = Path(args.request)
            if path.stat().st_size > 16 * 1024:
                raise ValueError("预约请求不能超过 16 KiB")
            payload = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(payload, dict) or not payload.get("id") or not payload.get("machineId"):
                raise ValueError("预约请求需包含固定 id 和 machineId；禁止自动生成替代 ID")
            client.capabilities.add("forwarded-resource-identity-v1")
            if payload.get("machineIds"):
                client.capabilities.add("atomic-group-reservations-v1")
            print(f"预约 ID：{payload['id']}", flush=True)
            show(client.api("/reservations", "POST", payload))
        elif args.action == "release-reservation":
            print(f"预约 ID：{args.id}", flush=True)
            show(client.api("/reservations/release", "POST", {"id": args.id}))
        else:
            result = client.api("/reservations")
            if args.id:
                if args.id not in result["reservations"]:
                    raise ValueError("预约不存在；未自动重放请求")
                result = result["reservations"][args.id]
            show(result)
        return 0
    if args.action == "ready":
        if args.group and args.machine:
            raise ValueError("ready 只能指定一个选择器")
        query = urllib.parse.urlencode({"group": args.group or "", "machineId": args.machine or ""})
        show(client.api("/ready?" + query))
    elif args.action == "machines":
        show(client.api("/machines"))
    elif args.action == "submit":
        if args.reservation_grant or args.reservation_generation:
            if not args.reservation or not args.reservation_grant or args.reservation_generation < 1:
                raise ValueError("整组提交须提供完整预约、Grant 和 generation")
            client.capabilities.update(("atomic-group-reservations-v1", "forwarded-resource-identity-v1"))
        if args.reservation and (not args.id or not args.machine or args.allow_legacy):
            raise ValueError("预约提交须提供原 --id 和固定 --machine，不能使用 legacy 模式")
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
        if args.reservation:
            client.capabilities.add("reservations")
            payload["reservationId"] = args.reservation
            if args.reservation_grant:
                payload["reservationGrantId"] = args.reservation_grant
                payload["reservationGeneration"] = args.reservation_generation
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
