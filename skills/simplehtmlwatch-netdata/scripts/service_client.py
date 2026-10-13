"""只读发现本机实例并校验每次请求身份。"""
import html
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        raise ValueError("不允许 HTTP 重定向")


OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
NO_RESTART = "禁止因探测失败自动重启、换版本、删锁或按进程名结束服务"


def base_url(raw):
    parts = urllib.parse.urlsplit(raw)
    if parts.scheme != "http" or parts.hostname not in ("127.0.0.1", "localhost"):
        raise ValueError("仅允许本机 HTTP 地址")
    if parts.username or parts.password or parts.path not in ("", "/") or parts.query or parts.fragment:
        raise ValueError("地址只能包含本机主机名和端口")
    if not parts.port or not 1 <= parts.port <= 65535:
        raise ValueError("请提供中台实际监听端口")
    return f"http://127.0.0.1:{parts.port}"


def service_dir(raw, instance):
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}", instance):
        raise ValueError("实例名需为 1–64 位字母、数字、下划线或连字符，首字符须为字母或数字")
    if raw:
        return Path(raw).expanduser().resolve()
    if sys.platform == "darwin":
        root = Path.home() / "Library" / "Application Support"
    elif os.name == "nt":
        if not os.environ.get("APPDATA"):
            raise ValueError("无法定位 APPDATA，请指定 --data-dir")
        root = Path(os.environ["APPDATA"])
    else:
        root = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    directory = root / "simpleHtmlWatch"
    if instance != "default":
        directory = directory / "instances" / instance
    return directory.resolve()


def request(url, token=None, run_id=None, timeout=25, limit=8 * 1024 * 1024):
    headers = {}
    if token:
        headers["X-Watch-Token"] = token
    if run_id:
        headers["X-Watch-Instance"] = run_id
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with OPENER.open(req, timeout=timeout) as response:
            raw = response.read(limit + 1)
            if len(raw) > limit:
                raise ValueError("响应超过读取上限，拒绝截断后继续使用")
            return raw.decode("utf-8")
    except urllib.error.HTTPError as exc:
        with exc:
            detail = exc.read(4096).decode("utf-8", "replace")
        try:
            detail = json.loads(detail).get("error", detail)
        except (ValueError, AttributeError):
            pass
        raise RuntimeError(f"HTTP {exc.code}：{detail}") from exc


class Client:
    """每次操作先发现，不启动或重放请求。"""

    def __init__(self, args):
        self.url_override = base_url(args.url) if args.url else None
        self.directory = service_dir(args.data_dir, args.instance)
        if self.url_override and args.data_dir:
            raise ValueError("--url 与 --data-dir 不能同时指定")
        self.instance, self.service_id = args.instance, args.service_id
        self.expect_version = args.expect_version
        self.required = set(args.require_capability)
        if args.action == "utilization":
            self.required.add("accelerator-utilization-v1")
            self.required.add("accelerator-occupancy-v1")
        elif args.action != "discover":
            self.required.add("netdata-metrics-v1")
        self.url = self.run_id = self.token = None

    def validate(self, info):
        if not isinstance(info, dict) or info.get("schema") != "simplehtmlwatch.service.v1" or info.get("apiVersion") != 1:
            raise RuntimeError(f"服务身份或 API 协议不兼容；{NO_RESTART}")
        for key in ("serviceId", "runId", "buildId", "dataDir", "version", "url"):
            if not isinstance(info.get(key), str) or not info[key]:
                raise RuntimeError(f"服务身份缺少 {key}")
        if info.get("instance") != self.instance:
            raise RuntimeError("实例名不匹配，拒绝串用实例")
        if self.expect_version and info["version"] != self.expect_version:
            raise RuntimeError(f"版本不匹配；{NO_RESTART}")
        caps = info.get("capabilities")
        if not isinstance(caps, list) or not all(isinstance(c, str) for c in caps):
            raise RuntimeError("能力列表无效")
        missing = self.required - set(caps)
        if missing:
            raise RuntimeError(f"缺少能力 {sorted(missing)}；{NO_RESTART}")

    def discover(self):
        expected = None
        if self.url_override:
            url = self.url_override
        else:
            path = self.directory / "service.json"
            try:
                expected = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise RuntimeError(f"无法读取实例登记 {path}；{NO_RESTART}") from exc
            self.validate(expected)
            if Path(expected["dataDir"]).resolve() != self.directory:
                raise RuntimeError("登记文件属于其他数据目录")
            url = base_url(expected["url"])
        try:
            live = json.loads(request(url + "/healthz", timeout=2, limit=65536))
        except (OSError, ValueError, RuntimeError) as exc:
            raise RuntimeError(f"实例身份查询失败：{exc}；{NO_RESTART}") from exc
        self.validate(live)
        if live["url"] != url:
            raise RuntimeError("在线实例声明了其他地址")
        if expected:
            for key in ("serviceId", "runId", "instance", "buildId", "version", "dataDir", "url"):
                if live.get(key) != expected.get(key):
                    raise RuntimeError(f"登记与在线 {key} 不一致；{NO_RESTART}")
        if self.service_id and live["serviceId"] != self.service_id:
            raise RuntimeError("serviceId 变化，拒绝连接其他中台")
        self.service_id = live["serviceId"]
        return live

    def api(self, path):
        if path.split("?", 1)[0] not in ("/api/config", "/api/status", "/api/metrics", "/api/utilization"):
            raise ValueError("只允许机器清单、状态、系统指标与利用率汇总接口")
        info = self.discover()
        url, run_id = info["url"], info["runId"]
        if self.token is None or (url, run_id) != (self.url, self.run_id):
            page = request(url + "/", run_id=run_id, limit=1024 * 1024)
            match = re.search(r'<meta\s+name="watch-token"\s+content="([^"]+)"', page)
            if not match:
                raise RuntimeError("首页没有会话令牌")
            self.token = html.unescape(match.group(1))
        self.url, self.run_id = url, run_id
        return json.loads(request(url + path, token=self.token, run_id=run_id))
