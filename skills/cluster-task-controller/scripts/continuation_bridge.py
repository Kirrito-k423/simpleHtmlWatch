#!/usr/bin/env python3
"""Deliver durable SHW events to their ORIGINAL Codex thread via an existing socket.

No daemon start/restart, new thread, fork, GUI typing or chat database writes.
The sender records an intent before turn/start; an uncertain send is queried,
never replayed. Only the bridge's own proxy child is ended on exit.
"""

import argparse
import base64
from datetime import datetime, timezone
import json
import hashlib
import os
from pathlib import Path
import queue
import socket
import struct
import subprocess
import sys
import threading
import time
import uuid

import taskctl


class RPCError(RuntimeError):
    """A definitive server error response, distinct from a lost response."""


class UnixWebSocket:
    """RFC 6455 client for Codex's private Unix socket; no extensions or auth export."""
    LIMIT = 16 * 1024 * 1024

    def __init__(self, path, timeout=20):
        if not hasattr(socket, "AF_UNIX"):
            raise ValueError("本机 Python 不支持 Unix socket；桌面原对话可使用 heartbeat 模式")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        self.buffer = bytearray()
        self.send_lock = threading.Lock()
        try:
            self.sock.connect(path)
            key = base64.b64encode(os.urandom(16)).decode("ascii")
            self.sock.sendall(("GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n"
                              "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
                              f"Sec-WebSocket-Key: {key}\r\n\r\n").encode("ascii"))
            while b"\r\n\r\n" not in self.buffer:
                data = self.sock.recv(4096)
                if not data:
                    raise RuntimeError("Codex socket closed during WebSocket upgrade")
                self.buffer.extend(data)
                if len(self.buffer) > 65536:
                    raise RuntimeError("WebSocket handshake exceeds limit")
            header, remaining = self.buffer.split(b"\r\n\r\n", 1)
            self.buffer = bytearray(remaining)
            lines = header.decode("ascii").split("\r\n")
            headers = {}
            for line in lines[1:]:
                name, value = line.split(":", 1)
                headers[name.lower()] = value.strip()
            expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode("ascii")).digest()).decode("ascii")
            status = lines[0].split()
            if len(status) < 2 or status[1] != "101" or headers.get("sec-websocket-accept") != expected or headers.get("upgrade", "").lower() != "websocket" or "upgrade" not in headers.get("connection", "").lower():
                raise RuntimeError("Invalid Codex WebSocket upgrade")
            self.sock.settimeout(None)  # Idle isn't failure; each RPC has its own deadline.
        except Exception:
            self.sock.close()
            raise

    def exact(self, length):
        while len(self.buffer) < length:
            data = self.sock.recv(min(65536, length - len(self.buffer)))
            if not data:
                raise RuntimeError("Codex WebSocket disconnected")
            self.buffer.extend(data)
        out = bytes(self.buffer[:length])
        del self.buffer[:length]
        return out

    def send(self, payload, opcode=1):
        data = payload.encode("utf-8") if isinstance(payload, str) else payload
        if len(data) > self.LIMIT or (opcode >= 8 and len(data) > 125):
            raise RuntimeError("WebSocket message exceeds limit")
        header = bytes([0x80 | opcode])
        length = len(data)
        if length < 126:
            header += bytes([0x80 | length])
        elif length <= 65535:
            header += bytes([0x80 | 126]) + struct.pack("!H", length)
        else:
            header += bytes([0x80 | 127]) + struct.pack("!Q", length)
        mask = os.urandom(4)
        masked = bytes(value ^ mask[i % 4] for i, value in enumerate(data))
        with self.send_lock:
            self.sock.sendall(header + mask + masked)

    def receive(self):
        fragments = bytearray()
        active = False
        while True:
            first, second = self.exact(2)
            fin, opcode, length = bool(first & 0x80), first & 0x0f, second & 0x7f
            if first & 0x70 or second & 0x80:
                raise RuntimeError("Unsupported WebSocket extensions or masked server frame")
            if opcode >= 8 and (not fin or length > 125):
                raise RuntimeError("Invalid WebSocket control frame")
            if length == 126:
                length = struct.unpack("!H", self.exact(2))[0]
            elif length == 127:
                length = struct.unpack("!Q", self.exact(8))[0]
            if length > self.LIMIT or len(fragments) + length > self.LIMIT:
                raise RuntimeError("WebSocket message exceeds limit")
            payload = self.exact(length)
            if opcode == 8:
                raise RuntimeError("Codex WebSocket closed")
            if opcode == 9:
                self.send(payload, opcode=10)
                continue
            if opcode == 10:
                continue
            if opcode == 1 and not active:
                active = True
            elif opcode != 0 or not active:
                raise RuntimeError("Unexpected WebSocket frame")
            fragments.extend(payload)
            if fin:
                return fragments.decode("utf-8")

    def close(self):
        try:
            self.sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self.sock.close()


class AppServer:
    def __init__(self, command=None, timeout=20, sock=None):
        self.timeout = timeout
        self.socket = UnixWebSocket(sock, timeout) if sock else None
        self.proc = None if sock else subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, text=True, encoding="utf-8",
                                     bufsize=1, shell=False)
        self.messages = queue.Queue()
        self.sequence = 0
        self.broken = False
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()
        try:
            self.call("initialize", {"clientInfo": {"name": "simplehtmlwatch_continuations",
                        "title": "simpleHtmlWatch experiment continuation", "version": "1"}})
            self.notify("initialized", {})
        except Exception:
            self.close()
            raise

    def _read(self):
        try:
            while True:
                line = self.socket.receive() if self.socket else self.proc.stdout.readline(16 * 1024 * 1024 + 1)
                if not line:
                    raise RuntimeError("Codex proxy disconnected")
                if len(line) > 16 * 1024 * 1024 or (not self.socket and not line.endswith("\n")):
                    raise RuntimeError("Codex protocol message exceeds limit")
                message = json.loads(line)
                if "id" in message:
                    self.messages.put(message)  # Don't retain streamed agent/tool notifications.
        except Exception as exc:
            self.broken = True
            self.messages.put(exc)

    def _send(self, message):
        if self.broken:
            raise RuntimeError("Codex connection lost; reconnect to existing socket")
        if self.socket:
            self.socket.send(json.dumps(message, ensure_ascii=False))
            return
        self.proc.stdin.write(json.dumps(message, ensure_ascii=False) + "\n")
        self.proc.stdin.flush()

    def notify(self, method, params):
        self._send({"method": method, "params": params})

    def call(self, method, params):
        self.sequence += 1
        request_id = self.sequence
        self._send({"id": request_id, "method": method, "params": params})
        deadline = time.monotonic() + self.timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                self.broken = True
                raise TimeoutError(f"Codex {method} response uncertain")
            try:
                message = self.messages.get(timeout=remaining)
            except queue.Empty as exc:
                self.broken = True
                raise TimeoutError(f"Codex {method} response uncertain") from exc
            if isinstance(message, Exception):
                raise message
            if "method" in message and "id" in message:
                # Approval requests are left for the real user/supported host UI.
                # This bridge never answers them on the user's behalf.
                continue
            if message.get("id") != request_id:
                continue
            if "error" in message:
                raise RPCError(str(message["error"]))
            if not isinstance(message.get("result"), dict):
                raise RuntimeError("Invalid Codex response")
            return message["result"]

    def archived(self, thread_id, workspace):
        cursor = None
        for _ in range(100):
            params = {"archived": True, "cwd": workspace, "limit": 100,
                      "sourceKinds": ["cli", "vscode", "exec", "appServer", "unknown",
                                      "subAgent", "subAgentReview", "subAgentCompact",
                                      "subAgentThreadSpawn", "subAgentOther"]}
            if cursor:
                params["cursor"] = cursor
            result = self.call("thread/list", params)
            if not isinstance(result.get("data"), list):
                raise RuntimeError("Archived thread inventory unavailable")
            if any(item.get("id") == thread_id for item in result["data"]):
                return True
            cursor = result.get("nextCursor")
            if not cursor:
                return False
        raise RuntimeError("Archived thread inventory exceeds scan limit")

    def read_thread(self, target, turns=False):
        thread = self.call("thread/read", {"threadId": target["threadId"], "includeTurns": turns}).get("thread")
        if not isinstance(thread, dict) or thread.get("id") != target["threadId"]:
            raise RuntimeError("Original thread identity mismatch")
        if not thread.get("cwd") or Path(thread["cwd"]).resolve() != Path(target["workspace"]).resolve():
            raise RuntimeError("Original thread workspace mismatch")
        return thread

    @staticmethod
    def find_event(thread, event_id):
        marker = f"[SHW_EVENT:{event_id}]"
        for turn in thread.get("turns", []):
            for item in turn.get("items", []):
                if item.get("type") != "userMessage":
                    continue
                for content in item.get("content", []):
                    if content.get("type") == "text" and marker in content.get("text", ""):
                        if turn.get("id"):
                            return turn["id"]
        return None

    def close(self):
        if self.socket:
            self.socket.close()
            return
        if self.proc.poll() is None:
            self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()  # The exact proxy child we created, never the daemon.
            self.proc.wait(timeout=5)
        for stream in (self.proc.stdin, self.proc.stdout):
            stream.close()


def utc_due(value):
    if not value:
        return True
    return datetime.fromisoformat(value.replace("Z", "+00:00")) <= datetime.now(timezone.utc)


def wake_prompt(job, service):
    event = job["wake"]
    binding = {"continuationId": job["id"], "generation": job["generation"],
               "wakeEventId": event["id"], "kind": event["kind"],
               "threadId": job["target"]["threadId"], "serviceId": service["serviceId"],
               "instance": service["instance"], "dataDir": service["dataDir"],
               "taskctlPath": str(Path(taskctl.__file__).resolve()),
               "skillPath": str(Path(__file__).resolve().parent.parent / "SKILL.md"),
               "checkpoint": job["checkpoint"]}
    return (f"[SHW_EVENT:{event['id']}]\n"
            "这是用户已授权的实验恢复事件。先读取下方绑定的 skillPath，使用对应版本 taskctlPath，重新发现绑定的中台实例，"
            "查询等待单、检查点和原任务 ID，不改变服务归属或运行版本。\n"
            "资源就绪时先核对检查点摘要并领取当前代资源，再通过 continuation-submit 提交固定计划；"
            "禁止换任务 ID、改命令或绕过中台 SSH 启动。事件过期、已完成或已暂停时结束本轮。\n"
            "任务结束事件应查询退出码、回收结果并按原实验判据判断；证据齐全后用 continuation-report complete 汇报。"
            "若未启动且无法继续，可 release-unused；存在启动意图或 unknown 时只查询恢复，不能超时释放。\n"
            "遵守原授权、预算、权限及项目指令，不重复请求原授权，不扩大实验范围。\n"
            + json.dumps(binding, ensure_ascii=False))


class Bridge:
    def __init__(self, client, app_server, host_id="local", sender_id=None, allow_cold_resume=False):
        self.client = client
        self.app = app_server
        self.host_id = host_id
        self.sender_id = sender_id or "bridge-" + uuid.uuid4().hex
        self.allow_cold_resume = allow_cold_resume

    def receipt(self, job, action, **fields):
        payload = {"id": job["id"], "generation": job["generation"],
                   "wakeEventId": job["wake"]["id"], "senderId": self.sender_id,
                   "action": action, **fields}
        return self.client.continuation_api("/delivery", "POST", payload)

    def process(self, job):
        wake, target = job.get("wake"), job["target"]
        if not wake or job["policy"]["wakeMode"] != "bridge" or target["hostId"] != self.host_id:
            return "skipped"
        if wake["status"] in ("sending", "delivery_uncertain"):
            # Negative history lookup isn't proof of non-delivery; do not replay.
            thread = self.app.read_thread(target, turns=True)
            turn_id = self.app.find_event(thread, wake["id"])
            if turn_id:
                self.receipt(job, "delivered", turnId=turn_id)
                return "delivery_recovered"
            return "delivery_uncertain"
        if wake["status"] != "pending" or not utc_due(wake.get("retryAt")):
            return "skipped"
        if job["state"] not in ("offered", "awaiting_review") or job.get("stopRequested"):
            return "skipped"
        if job["state"] == "offered" and utc_due(job.get("offerUntil")):
            return "offer_expired"
        thread = self.app.read_thread(target)
        status = thread.get("status", {}).get("type")
        if status == "notLoaded" and not self.allow_cold_resume:
            self.receipt(job, "defer", reason="original_thread_not_loaded_in_bound_host; use original host heartbeat")
            return "deferred"
        if status not in ("idle", "notLoaded"):
            self.receipt(job, "defer", reason="original_thread_" + str(status))
            return "deferred"
        if self.app.archived(target["threadId"], target["workspace"]):
            self.receipt(job, "defer", reason="original_thread_archived")
            return "deferred"
        taskctl.checkpoint_digest(job["checkpoint"])
        resumed = self.app.call("thread/resume", {"threadId": target["threadId"]}).get("thread", {})
        if resumed.get("id") != target["threadId"] or not resumed.get("cwd") or Path(resumed["cwd"]).resolve() != Path(target["workspace"]).resolve():
            raise RuntimeError("Resumed thread identity/workspace mismatch")
        if resumed.get("status", {}).get("type") != "idle":
            self.receipt(job, "defer", reason="original_thread_busy_after_resume")
            return "deferred"
        sending = self.receipt(job, "begin")  # Durable intent before external write.
        try:
            service = self.client.discover()
            result = self.app.call("turn/start", {"threadId": target["threadId"],
                                   "input": [{"type": "text", "text": wake_prompt(sending, service)}]})
            turn_id = result.get("turn", {}).get("id")
            if not turn_id:
                raise RuntimeError("turn/start returned no turn ID")
        except RPCError as exc:
            self.receipt(sending, "not-sent", reason=str(exc)[:1024])
            return "rejected"
        except Exception as exc:
            self.receipt(sending, "uncertain", reason=str(exc)[:1024])
            return "delivery_uncertain"
        # If this write's receipt is lost, the persisted sending state and event
        # marker make the next run QUERY the accepted turn, not start another.
        self.receipt(sending, "delivered", turnId=turn_id)
        return "delivered"

    def run_once(self):
        result = self.client.continuation_api()
        outcomes = []
        for job in result["continuations"]:
            try:
                state = self.process(job)
            except (ValueError, RuntimeError, OSError, TimeoutError) as exc:
                if job.get("wake", {}).get("status") == "pending":
                    try:
                        self.receipt(job, "defer", reason=str(exc)[:1024])
                    except (ValueError, RuntimeError, OSError, TimeoutError):
                        pass  # A concurrent sender/claim or uncertain write owns recovery.
                outcomes.append({"id": job["id"], "status": "deferred", "reason": str(exc)[:1024]})
            else:
                if state != "skipped":
                    outcomes.append({"id": job["id"], "status": state})
        return outcomes


def main():
    parser = argparse.ArgumentParser(description="连接已有 Codex 控制 socket，续接已绑定的原实验对话")
    taskctl.client_arguments(parser)
    parser.add_argument("--sock", required=True, help="已有 App Server 控制 socket；本程序不启动 daemon")
    parser.add_argument("--host-id", default="local")
    parser.add_argument("--interval", type=float, default=10)
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--allow-cold-resume", action="store_true", help="明确允许在选定宿主续接未加载对话；需确认没有其他宿主持有它")
    parser.add_argument("--probe-thread", help="只读核对原对话，不发送事件")
    parser.add_argument("--workspace", help="只读探测的准确工作目录")
    args = parser.parse_args()
    if args.interval < 1 or args.allow_legacy:
        raise ValueError("检查周期至少 1 秒，桥接不允许 legacy 模式")
    if not Path(args.sock).is_absolute():
        raise ValueError("控制 socket 必须使用明确的绝对路径")
    args.action = "discover"
    client = taskctl.Client(args)
    if args.probe_thread and not args.workspace:
        raise ValueError("只读探测需同时提供 --workspace")
    app = None
    try:
        if args.probe_thread:
            app = AppServer(sock=args.sock)
            thread = app.read_thread({"threadId": args.probe_thread, "workspace": args.workspace})
            taskctl.show({key: thread.get(key) for key in ("id", "cwd", "status", "cliVersion")})
            return 0
        bridge = None
        last_outcomes = None
        while True:
            try:
                if app is None or app.broken:
                    # Discover the SAME SHW identity; reconnect only to the
                    # explicit existing socket. Never start either service.
                    client.continuation_api()
                    if app:
                        app.close()
                        app = None
                    app = AppServer(sock=args.sock)
                    if bridge is None:
                        bridge = Bridge(client, app, args.host_id, allow_cold_resume=args.allow_cold_resume)
                    else:
                        bridge.app = app
                outcomes = bridge.run_once()
            except (ValueError, RuntimeError, OSError, TimeoutError) as exc:
                if args.once:
                    raise
                outcomes = [{"status": "waiting_for_service_or_host", "reason": str(exc)[:1024]}]
            if outcomes and outcomes != last_outcomes:
                taskctl.show(outcomes)
            last_outcomes = outcomes
            if args.once:
                return 0
            time.sleep(args.interval)
    finally:
        if app:
            app.close()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(0)
    except (ValueError, RuntimeError, OSError, TimeoutError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        sys.exit(2)
