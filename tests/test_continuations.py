"""Continuation protocol fixtures. No real AI messages or remote SSH machines."""
import argparse
import base64
import copy
import contextlib
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import socket
import struct
import sys
import tempfile
import threading
import time
import unittest

SCRIPTS = Path(__file__).resolve().parents[1] / "skills/cluster-task-controller/scripts"
sys.path.insert(0, str(SCRIPTS))
import taskctl
import continuation_bridge as bridge
sys.path.remove(str(SCRIPTS))


class FakeHost(bridge.AppServer):
    def __init__(self, target):
        self.thread = {"id": target["threadId"], "cwd": target["workspace"],
                       "status": {"type": "idle"}, "turns": []}
        self.starts = 0
        self.resumes = 0
        self.is_archived = False
        self.mode = "ok"

    def call(self, method, params):
        if method == "thread/read":
            return {"thread": copy.deepcopy(self.thread)}
        if method == "thread/list":
            return {"data": [self.thread] if self.is_archived else [], "nextCursor": None}
        if method == "thread/resume":
            self.resumes += 1
            return {"thread": copy.deepcopy(self.thread)}
        if method == "turn/start":
            if self.mode == "rejected":
                raise bridge.RPCError("turn not accepted")
            self.starts += 1
            self.thread["turns"].append({"id": "turn-one", "items": [
                {"type": "userMessage", "content": params["input"]}]})
            if self.mode == "lost":
                raise TimeoutError("accepted turn response lost")
            return {"turn": {"id": "turn-one", "status": "inProgress"}}
        raise AssertionError(method)


class FakeClient:
    def __init__(self, job):
        self.job = copy.deepcopy(job)
        self.calls = []
        self.mu = threading.Lock()
        self.lose_receipt = False

    def discover(self):
        return {"serviceId": "test-service", "instance": "canary",
                "dataDir": "/temporary-test-instance"}

    def continuation_api(self, path="", method="GET", data=None):
        with self.mu:
            if method == "GET":
                return {"continuations": [copy.deepcopy(self.job)]}
            self.calls.append(copy.deepcopy(data))
            wake = self.job["wake"]
            action = data["action"]
            if action == "begin":
                if wake["status"] != "pending":
                    raise taskctl.HTTPFailure(409, "already sending")
                wake["status"] = "sending"
                wake["senderId"] = data["senderId"]
            elif action == "delivered":
                if self.lose_receipt:
                    self.lose_receipt = False
                    raise RuntimeError("SHW receipt connection lost before commit")
                wake["status"] = "delivered"
                wake["turnId"] = data["turnId"]
            elif action == "uncertain":
                wake["status"] = "delivery_uncertain"
            elif action == "not-sent":
                wake["status"] = "pending"
                wake["retryAt"] = (datetime.now(timezone.utc) + timedelta(minutes=1)).isoformat()
            elif action == "defer":
                wake["reason"] = data["reason"]
            else:
                raise AssertionError(action)
            return copy.deepcopy(self.job)


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        checkpoint = Path(self.temp.name) / "checkpoint.json"
        checkpoint.write_text('{"next":"authorized-step"}')
        self.job = {"id": "wait-one", "generation": 1, "state": "offered",
            "policy": {"wakeMode": "bridge"}, "checkpoint": {"path": str(checkpoint),
                "digest": hashlib.sha256(checkpoint.read_bytes()).hexdigest()},
            "target": {"provider": "codex", "hostId": "local", "threadId": "original-thread",
                "workspace": self.temp.name}, "wake": {"id": "event-one", "kind": "resources_ready",
                "status": "pending"},
            "offerUntil": (datetime.now(timezone.utc) + timedelta(minutes=5)).isoformat()}
        self.client = FakeClient(self.job)
        self.host = FakeHost(self.job["target"])
        self.sender = bridge.Bridge(self.client, self.host)

    def test_wakes_exact_thread_and_durable_receipt_once(self):
        self.assertEqual(self.sender.run_once()[0]["status"], "delivered")
        self.assertEqual(self.host.starts, 1)
        self.assertEqual(self.client.calls[0]["action"], "begin")
        self.assertEqual(self.client.calls[-1]["action"], "delivered")
        content = self.host.thread["turns"][0]["items"][0]["content"][0]["text"]
        self.assertIn("[SHW_EVENT:event-one]", content)
        self.assertNotIn("X-Watch-Token", content)
        self.sender.run_once()
        self.assertEqual(self.host.starts, 1)

    def test_lost_turn_response_recovers_by_query_never_replays(self):
        self.host.mode = "lost"
        self.assertEqual(self.sender.run_once()[0]["status"], "delivery_uncertain")
        restarted = bridge.Bridge(self.client, self.host, sender_id="new-process")
        self.assertEqual(restarted.run_once()[0]["status"], "delivery_recovered")
        self.assertEqual(self.host.starts, 1)
        self.assertEqual(self.client.job["wake"]["turnId"], "turn-one")

    def test_lost_shw_receipt_queries_accepted_turn(self):
        self.client.lose_receipt = True
        self.sender.run_once()
        self.assertEqual(self.client.job["wake"]["status"], "sending")
        self.assertEqual(self.sender.run_once()[0]["status"], "delivery_recovered")
        self.assertEqual(self.host.starts, 1)

    def test_negative_history_is_not_non_delivery_proof(self):
        self.client.job["wake"]["status"] = "delivery_uncertain"
        for _ in range(3):
            self.assertEqual(self.sender.run_once()[0]["status"], "delivery_uncertain")
        self.assertEqual(self.host.starts, 0)
        self.assertEqual(self.client.calls, [])

    def test_active_archived_or_changed_checkpoint_does_not_wake(self):
        self.host.thread["status"] = {"type": "active", "activeFlags": ["waitingOnApproval"]}
        self.assertEqual(self.sender.run_once()[0]["status"], "deferred")
        self.host.thread["status"] = {"type": "idle"}
        self.host.is_archived = True
        self.assertEqual(self.sender.run_once()[0]["status"], "deferred")
        self.host.is_archived = False
        Path(self.job["checkpoint"]["path"]).write_text("changed")
        self.assertEqual(self.sender.run_once()[0]["status"], "deferred")
        self.assertEqual(self.host.starts, 0)
        self.assertEqual(self.host.resumes, 0)

    def test_workspace_identity_and_host_binding(self):
        self.host.thread["cwd"] = "/different-workspace"
        self.sender.run_once()
        self.assertEqual(self.host.starts, 0)
        self.client.job["target"]["hostId"] = "different-host"
        self.assertEqual(self.sender.run_once(), [])

    def test_readable_history_is_not_proof_of_owning_live_desktop_thread(self):
        self.host.thread["status"] = {"type": "notLoaded"}
        self.assertEqual(self.sender.run_once()[0]["status"], "deferred")
        self.assertEqual(self.host.resumes, 0)
        self.assertEqual(self.host.starts, 0)

    def test_two_bridges_cannot_begin_same_event(self):
        workers = [threading.Thread(target=self.sender.run_once) for _ in range(2)]
        for worker in workers:
            worker.start()
        for worker in workers:
            worker.join()
        self.assertEqual(self.host.starts, 1)

    def test_definitive_rejection_backs_off_without_uncertain_replay(self):
        self.host.mode = "rejected"
        self.assertEqual(self.sender.run_once()[0]["status"], "rejected")
        self.assertEqual(self.client.calls[-1]["action"], "not-sent")
        self.assertEqual(self.sender.run_once(), [])
        self.assertEqual(self.host.starts, 0)

    def test_task_completion_event_uses_same_original_thread(self):
        self.client.job["state"] = "awaiting_review"
        self.client.job["wake"]["kind"] = "tasks_finished"
        self.client.job.pop("offerUntil")
        self.assertEqual(self.sender.run_once()[0]["status"], "delivered")
        self.assertEqual(self.host.thread["id"], "original-thread")


class AppServerTransportTests(unittest.TestCase):
    @unittest.skipUnless(hasattr(socket, "AF_UNIX") and os.name != "nt", "Unix socket bridge fixture")
    def test_real_websocket_upgrade_masking_ping_and_fragmented_rpc(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "codex.sock")
            listener = socket.socket(socket.AF_UNIX)
            listener.bind(path)
            listener.listen(1)
            errors, methods = [], []
            def serve():
                try:
                    conn, _ = listener.accept()
                    conn.settimeout(3)
                    with conn:
                        data = b""
                        while b"\r\n\r\n" not in data:
                            data += conn.recv(4096)
                        key = next(line.split(b":", 1)[1].strip() for line in data.split(b"\r\n") if line.lower().startswith(b"sec-websocket-key:"))
                        accept = base64.b64encode(hashlib.sha1(key + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest())
                        conn.sendall(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + b"\r\n\r\n")
                        def exact(n):
                            data = b""
                            while len(data) < n:
                                block = conn.recv(n - len(data))
                                if not block:
                                    raise EOFError()
                                data += block
                            return data
                        def frame(payload, opcode=1, fin=True):
                            body = payload if isinstance(payload, bytes) else payload.encode()
                            n = len(body)
                            header = bytes([(0x80 if fin else 0) | opcode])
                            header += bytes([n]) if n < 126 else bytes([126]) + struct.pack("!H", n)
                            conn.sendall(header + body)
                        while True:
                            try:
                                first, second = exact(2)
                            except EOFError:
                                break
                            if not second & 0x80:
                                raise AssertionError("client frame unmasked")
                            n = second & 0x7f
                            if n == 126: n = struct.unpack("!H", exact(2))[0]
                            if n == 127: n = struct.unpack("!Q", exact(8))[0]
                            mask, body = exact(4), exact(n)
                            body = bytes(value ^ mask[i % 4] for i, value in enumerate(body))
                            if first & 0x0f == 10:
                                methods.append("pong")
                                continue
                            request = json.loads(body)
                            methods.append(request["method"])
                            if "id" not in request:
                                continue
                            result = {"ok": True} if request["method"] == "initialize" else {"thread": {"id": "original", "cwd": directory, "status": {"type": "idle"}}}
                            response = json.dumps({"id": request["id"], "result": result}).encode()
                            frame(b"heartbeat", opcode=9)
                            frame(response[:7], fin=False)
                            frame(response[7:], opcode=0)
                except Exception as exc:
                    errors.append(exc)
            worker = threading.Thread(target=serve, daemon=True)
            worker.start()
            client = bridge.AppServer(sock=path, timeout=2)
            try:
                thread = client.read_thread({"threadId": "original", "workspace": directory})
                self.assertEqual(thread["id"], "original")
            finally:
                client.close()
                worker.join(timeout=4)
                listener.close()
            self.assertFalse(worker.is_alive())
            self.assertEqual(errors, [])
            self.assertIn("initialized", methods)
            self.assertIn("pong", methods)
    def test_stdio_rpc_and_own_child_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            program = Path(directory) / "fake_host.py"
            program.write_text('''import json, sys
for line in sys.stdin:
 m=json.loads(line)
 if 'id' in m:
  print(json.dumps({'id':m['id'],'result':{'ok':True}}),flush=True)
''')
            client = bridge.AppServer([sys.executable, "-u", str(program)], timeout=2)
            try:
                self.assertEqual(client.call("test/read", {}), {"ok": True})
            finally:
                client.close()
            self.assertIsNotNone(client.proc.poll())

    def test_event_recovery_requires_a_user_input_marker(self):
        thread = {"turns": [{"id": "turn", "items": [{"type": "agentMessage",
                    "text": "[SHW_EVENT:event-one]"}]}]}
        self.assertIsNone(bridge.AppServer.find_event(thread, "event-one"))


class ContinuationClientTests(unittest.TestCase):
    def test_changed_checkpoint_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cp.json"
            path.write_text("checkpoint")
            self.assertEqual(taskctl.checkpoint_digest({"path": str(path)}),
                             hashlib.sha256(path.read_bytes()).hexdigest())
            with self.assertRaises(ValueError):
                taskctl.checkpoint_digest({"path": str(path), "digest": "0" * 64})

    def test_continuation_cli_arguments_do_not_replace_command_name(self):
        parser = argparse.ArgumentParser()
        sub = parser.add_subparsers(dest="action", required=True)
        taskctl.continuation_actions(sub)
        args = parser.parse_args(["continuation-control", "id", "pause"])
        self.assertEqual((args.action, args.action_name), ("continuation-control", "pause"))
        args = parser.parse_args(["continuation-report", "id", "--thread", "t", "--generation", "1",
                "--event", "event", "--action", "complete", "--outcome", "succeeded", "--evidence", "checked"])
        self.assertEqual((args.action, args.report_action), ("continuation-report", "complete"))

    def test_legacy_mode_rejected_before_discovery(self):
        client = taskctl.Client(argparse.Namespace(url="http://127.0.0.1:9999", data_dir=None,
            instance="default", allow_legacy=True, expect_version=None, require_capability=[],
            service_id=None, action="discover"))
        with self.assertRaises(ValueError):
            client.continuation_api()


class ContinuationEndToEndTests(unittest.TestCase):
    def test_go_http_bridge_original_thread_task_receipts_and_release(self):
        root = SCRIPTS.parents[2]
        with tempfile.TemporaryDirectory(prefix="shw-continuation-e2e-") as directory:
            discovery = Path(directory) / "fixture-service.json"
            env = dict(os.environ, SHW_CONTINUATION_FIXTURE="1", SHW_CONTINUATION_DISCOVERY=str(discovery))
            proc = subprocess.Popen(["go", "test", "./internal/watch", "-run", "^TestContinuationIntegrationFixture$", "-count=1", "-v"], cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8")
            client = None
            try:
                deadline = time.monotonic() + 30
                while not discovery.exists():
                    if proc.poll() is not None:
                        self.fail("fixture exited: " + proc.communicate()[0])
                    if time.monotonic() > deadline:
                        self.fail("fixture startup timeout")
                    time.sleep(.05)
                info = json.loads(discovery.read_text())
                args = argparse.Namespace(url=info["url"], data_dir=None, instance="test", service_id=info["serviceId"], expect_version="fixture", require_capability=[], allow_legacy=False, action="discover")
                client = taskctl.Client(args)
                checkpoint = Path(directory) / "checkpoint.json"
                checkpoint.write_text('{"goal":"synthetic protocol fixture"}')
                request = {"id":"e2e-wait","experimentId":"e2e-exp","stepId":"step1",
                    "target":{"provider":"codex","hostId":"local","threadId":"original-fixture-thread","workspace":directory},
                    "resources":{"group":"gpu","machineCount":2,"model":"910B","devicesPerMachine":2,"minFreeHbmMB":60000,"maxUtilization":5},
                    "checkpoint":{"path":str(checkpoint),"digest":taskctl.checkpoint_digest({"path":str(checkpoint)})},
                    "authorizationRef":"fixture-only-authorization","authorized":True,
                    "tasks":[{"id":"e2e-task1","shell":"printf one"},{"id":"e2e-task2","shell":"printf two"}],
                    "existingTaskIds":[]}
                initial = client.continuation_api("", "POST", request)
                self.assertEqual(initial["state"], "waiting_resources")
                self.assertEqual(client.continuation_api("", "POST", request)["id"], request["id"])
                def wait_state(state):
                    deadline = time.monotonic() + 8
                    while time.monotonic() < deadline:
                        current = client.continuation_api("?id=e2e-wait")
                        if current["state"] == state:
                            return current
                        time.sleep(.05)
                    self.fail("state not reached: " + json.dumps(current))
                offered = wait_state("offered")
                host = FakeHost(request["target"])
                sender = bridge.Bridge(client, host)
                self.assertEqual(sender.run_once()[0]["status"], "delivered")
                claim = {"id":request["id"],"generation":offered["generation"],"wakeEventId":offered["wake"]["id"],"threadId":request["target"]["threadId"],"checkpointDigest":request["checkpoint"]["digest"]}
                claimed = client.continuation_api("/claim", "POST", claim)
                submit_args = argparse.Namespace(action="continuation-submit",id=request["id"],thread=request["target"]["threadId"],generation=claimed["generation"],grant=claimed["grantId"])
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(taskctl.run_continuation_action(client,submit_args),0)
                    self.assertEqual(taskctl.run_continuation_action(client,submit_args),0)
                review = wait_state("awaiting_review")
                self.assertEqual(review["wake"]["kind"], "tasks_finished")
                self.assertEqual(sender.run_once()[0]["status"], "delivered")
                claim["wakeEventId"] = review["wake"]["id"]
                report = dict(claim,action="complete",outcome="succeeded",evidence="synthetic fixture: both mock tasks returned zero and archives collected")
                self.assertEqual(client.continuation_api("/report","POST",report)["state"], "completed")
                self.assertEqual(len(client.api("/ready")),2)
                count = json.loads(taskctl.request(info["url"]+"/fixture/count"))
                self.assertEqual(count["launches"],2)
                self.assertEqual(host.starts,2)
                self.assertEqual(sender.run_once(),[])
            finally:
                if discovery.exists():
                    try:
                        info = json.loads(discovery.read_text())
                        taskctl.request(info["url"]+"/fixture/stop",method="POST",data={},timeout=2)
                    except (ValueError,RuntimeError,OSError):
                        pass
                try:
                    output,_ = proc.communicate(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.terminate()
                    output,_ = proc.communicate(timeout=5)
                self.assertEqual(proc.returncode,0,output)


if __name__ == "__main__":
    unittest.main()
