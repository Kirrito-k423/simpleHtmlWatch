"""Local process/HTTP regressions; fresh empty stores, no remote SSH or pkill."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
CLIENT = ROOT / "skills/cluster-task-controller/scripts/taskctl.py"
spec = importlib.util.spec_from_file_location("taskctl", CLIENT)
taskctl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(taskctl)


class LifecycleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.builds = tempfile.TemporaryDirectory(prefix="shw-builds-")
        cls.addClassCleanup(cls.builds.cleanup)
        cls.binaries = {}
        for version in ("test-v1", "test-v2", "test-v1-rebuilt"):
            binary = Path(cls.builds.name) / (version + (".exe" if os.name == "nt" else ""))
            build_version = "test-v1" if version == "test-v1-rebuilt" else version
            build_flags = ["-trimpath"] if version == "test-v1-rebuilt" else []
            subprocess.run(["go", "build", *build_flags, "-o", str(binary), "-ldflags", f"-X main.version={build_version}", "."], cwd=ROOT, check=True)
            cls.binaries[version] = binary

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="shw-instances-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name) / "数据 dir"

    def launch(self, version="test-v1", directory=None, extra=()):
        directory = directory or self.directory
        proc = subprocess.Popen([str(self.binaries[version]), "-no-browser", "-data-dir", str(directory), *extra], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8")
        self.addCleanup(self.stop, proc)
        return proc

    def stop(self, proc):
        if proc.poll() is None:
            proc.terminate()
        try:
            output, _ = proc.communicate(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            output, _ = proc.communicate(timeout=5)
            self.fail(f"test process did not stop: {output}")
        return output

    def running(self, proc, directory=None):
        directory = directory or self.directory
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                self.fail(f"process exited before ready: {proc.communicate()[0]}")
            try:
                info = json.loads((directory / "service.json").read_text(encoding="utf-8"))
                if info["pid"] == proc.pid:
                    with taskctl.OPENER.open(info["url"] + "/healthz", timeout=.3) as response:
                        live = json.load(response)
                    if live == info:
                        return live
            except (OSError, ValueError):
                pass
            time.sleep(.02)
        self.fail("test instance did not become ready")

    def cli(self, *args, directory=None, version="test-v1"):
        return subprocess.run([str(self.binaries[version]), "-no-browser", "-data-dir", str(directory or self.directory), *args], capture_output=True, text=True, encoding="utf-8", timeout=20)

    def client(self, directory=None, **kwargs):
        args = dict(url=None, data_dir=str(directory or self.directory), instance="default", allow_legacy=False, expect_version=None, require_capability=[], action="ready", service_id=None)
        args.update(kwargs)
        return taskctl.Client(argparse.Namespace(**args))

    def test_duplicate_start_reuses_owner_even_with_other_version_and_port(self):
        owner = self.launch()
        before = self.running(owner)
        result = self.cli("-port", "0", "-adopt-build", version="test-v2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(before["url"], result.stdout)
        self.assertIn("test-v1", result.stdout)
        self.assertEqual(self.running(owner), before)

    def test_concurrent_first_launch_has_exactly_one_owner(self):
        first, second = self.launch(), self.launch(version="test-v2")
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and first.poll() is None and second.poll() is None:
            time.sleep(.02)
        alive = [p for p in (first, second) if p.poll() is None]
        self.assertEqual(len(alive), 1)
        loser = second if alive[0] is first else first
        output = loser.communicate(timeout=2)[0]
        self.assertEqual(loser.returncode, 0, output)
        info = self.running(alive[0])
        self.assertIn(info["url"], output)

    def test_restart_reuses_port_and_identity_but_rotates_run_and_token(self):
        first = self.launch()
        before = self.running(first)
        client = self.client()
        client.prepare()
        token = client.token
        self.stop(first)
        second = self.launch()
        after = self.running(second)
        self.assertEqual(after["url"], before["url"])
        self.assertEqual(after["serviceId"], before["serviceId"])
        self.assertNotEqual(after["runId"], before["runId"])
        client.api("/ready")
        self.assertNotEqual(client.token, token)

    def test_client_rediscovers_deliberate_port_migration_without_restart(self):
        first = self.launch()
        before = self.running(first)
        client = self.client()
        client.api("/ready")
        self.stop(first)
        # Occupy the old port so -port 0 must choose a different one.
        with socket.socket() as blocker:
            blocker.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            blocker.bind(("127.0.0.1", int(before["url"].rsplit(":", 1)[1])))
            blocker.listen()
            second = self.launch(extra=("-port", "0"))
            after = self.running(second)
            self.assertNotEqual(after["url"], before["url"])
            client.api("/ready")
            self.assertEqual(client.url, after["url"])
            self.assertEqual(client.service_id, before["serviceId"])

    def test_busy_persisted_port_does_not_move_or_mutate_registry(self):
        first = self.launch()
        info = self.running(first)
        self.stop(first)
        record = (self.directory / "service.json").read_bytes()
        with socket.socket() as blocker:
            blocker.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            blocker.bind(("127.0.0.1", int(info["url"].rsplit(":", 1)[1])))
            blocker.listen()
            result = self.cli()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("不会换端口", result.stderr)
            self.assertEqual(record, (self.directory / "service.json").read_bytes())

    def test_build_switch_requires_explicit_adoption_and_blocks_old_build(self):
        first = self.launch()
        before = self.running(first)
        self.stop(first)
        result = self.cli(version="test-v2")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("-adopt-build", result.stderr)
        second = self.launch(version="test-v2", extra=("-adopt-build",))
        after = self.running(second)
        self.assertEqual(before["url"], after["url"])
        self.assertEqual(before["serviceId"], after["serviceId"])
        self.assertNotEqual(before["buildId"], after["buildId"])
        self.stop(second)
        self.assertNotEqual(self.cli().returncode, 0)

    def test_same_version_label_does_not_bypass_build_pin(self):
        first = self.launch()
        self.running(first)
        self.stop(first)
        result = self.cli(version="test-v1-rebuilt")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("已固定构建 test-v1", result.stderr)

    def test_unrelated_http_service_on_old_port_is_not_taken_over(self):
        first = self.launch()
        info = self.running(first)
        self.stop(first)
        class Unrelated(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"status":"ok","app":"unrelated"}')
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", int(info["url"].rsplit(":", 1)[1])), Unrelated)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with self.assertRaisesRegex(RuntimeError, "协议不兼容"):
                self.client().discover()
            self.assertNotEqual(self.cli("-status").returncode, 0)
            self.assertNotEqual(self.cli().returncode, 0)
            with taskctl.OPENER.open(info["url"] + "/healthz") as response:
                self.assertEqual(json.load(response)["app"], "unrelated")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def test_two_versions_coexist_with_independent_data_and_identity(self):
        first = self.launch()
        before = self.running(first)
        other_dir = Path(self.temp.name) / "canary"
        second = self.launch(version="test-v2", directory=other_dir, extra=("-instance", "canary"))
        after = self.running(second, other_dir)
        self.assertNotEqual(before["url"], after["url"])
        self.assertNotEqual(before["serviceId"], after["serviceId"])
        for info in (before, after):
            with taskctl.OPENER.open(info["url"] + "/") as response:
                page = response.read().decode("utf-8")
            self.assertIn(f'<title>simpleHtmlWatch {info["version"]}', page)
            self.assertIn(f'构建：{info["buildId"][:12]}', page)
            self.assertIn(f'实例：{info["instance"]}', page)
            self.assertIn(f'>{info["version"]}</span>', page)
            self.assertNotIn("__WATCH_", page)
        self.client(other_dir, instance="canary").api("/ready")
        with self.assertRaisesRegex(RuntimeError, "串用"):
            self.client(other_dir).discover()
        self.assertEqual(self.running(first), before)

    def test_stale_record_is_not_health_and_status_does_not_start_anything(self):
        owner = self.launch()
        before = self.running(owner)
        self.assertEqual(json.loads(self.cli("-status").stdout), before)
        self.stop(owner)
        self.assertNotEqual(self.cli("-status").returncode, 0)
        with self.assertRaisesRegex(RuntimeError, "禁止"):
            self.client().discover()
        self.assertEqual(json.loads((self.directory / "service.json").read_text(encoding="utf-8")), before)

    def test_status_missing_directory_has_no_side_effects(self):
        self.assertNotEqual(self.cli("-status").returncode, 0)
        self.assertFalse(self.directory.exists())

    def test_corrupt_registry_is_not_silently_overwritten(self):
        self.directory.mkdir()
        path = self.directory / "service.json"
        path.write_text("{broken", encoding="utf-8")
        self.assertNotEqual(self.cli().returncode, 0)
        self.assertEqual(path.read_text(encoding="utf-8"), "{broken")
        self.assertFalse((self.directory / "vault.key").exists())

    def test_health_identity_host_origin_and_stale_request_fence(self):
        owner = self.launch()
        info = self.running(owner)
        self.assertNotIn("token", json.dumps(info).lower())
        client = self.client()
        client.prepare()
        for headers in ({"Host": "evil.test"}, {"Origin": "https://evil.test"}):
            with self.assertRaises(urllib.error.HTTPError) as error:
                taskctl.OPENER.open(urllib.request.Request(info["url"] + "/healthz", headers=headers))
            self.assertEqual(error.exception.code, 403)
            error.exception.close()
        with self.assertRaises(taskctl.HTTPFailure) as error:
            taskctl.request(info["url"] + "/api/tasks", client.token, "POST", {"id": "not-submitted", "shell": "true"}, run_id="obsolete-run")
        self.assertEqual(error.exception.status, 409)
        self.assertFalse((self.directory / "tasks/not-submitted").exists())

    def test_wrong_identity_version_and_capability_are_not_outages(self):
        owner = self.launch()
        info = self.running(owner)
        for kwargs, message in (({"service_id": "other"}, "serviceId"), ({"expect_version": "v9"}, "版本不符"), ({"require_capability": ["missing"]}, "缺少能力")):
            with self.assertRaisesRegex(RuntimeError, message):
                self.client(**kwargs).discover()
        record = dict(info, runId="wrong-run")
        (self.directory / "service.json").write_text(json.dumps(record), encoding="utf-8")
        with self.assertRaisesRegex(RuntimeError, "不一致"):
            self.client().discover()
        self.assertIsNone(owner.poll())

    def test_copied_registry_cannot_select_another_data_directory(self):
        owner = self.launch()
        self.running(owner)
        other = Path(self.temp.name) / "copied"
        other.mkdir()
        (other / "service.json").write_bytes((self.directory / "service.json").read_bytes())
        with self.assertRaisesRegex(RuntimeError, "其他数据目录"):
            self.client(other).discover()
        self.assertNotEqual(self.cli("-status", directory=other).returncode, 0)

    def test_real_client_cli_discovers_without_url_and_rejects_wrong_explicit_url(self):
        owner = self.launch()
        info = self.running(owner)
        env = {k: v for k, v in os.environ.items() if k not in ("SHW_URL", "SHW_INSTANCE", "SHW_DATA_DIR")}
        env["PYTHONIOENCODING"] = "utf-8"
        output = subprocess.run([sys.executable, str(CLIENT), "--data-dir", str(self.directory), "discover"], capture_output=True, text=True, encoding="utf-8", env=env)
        self.assertEqual(output.returncode, 0, output.stderr)
        self.assertEqual(json.loads(output.stdout), info)
        with self.assertRaisesRegex(ValueError, "不能同时"):
            self.client(url=info["url"])


class ClientSafetyTests(unittest.TestCase):
    def args(self, **kwargs):
        args = dict(url="http://127.0.0.1:12345", data_dir=None, instance="default", allow_legacy=False, expect_version=None, require_capability=[], action="ready", service_id=None)
        args.update(kwargs)
        return argparse.Namespace(**args)

    def test_legacy_needs_explicit_opt_in_and_does_not_mask_newer_protocol(self):
        with patch.object(taskctl, "request", side_effect=taskctl.HTTPFailure(404, "old")):
            with self.assertRaisesRegex(RuntimeError, "禁止"):
                taskctl.Client(self.args()).discover()
            self.assertTrue(taskctl.Client(self.args(allow_legacy=True)).discover()["legacy"])
        with patch.object(taskctl, "request", return_value=json.dumps({"schema": taskctl.SERVICE_SCHEMA, "apiVersion": 999})):
            with self.assertRaisesRegex(RuntimeError, "协议不兼容"):
                taskctl.Client(self.args(allow_legacy=True)).discover()

    def test_uncertain_post_is_never_replayed(self):
        client = taskctl.Client(self.args())
        client.url, client.token, client.run_id = "http://127.0.0.1:12345", "token", "run"
        with patch.object(client, "prepare"), patch.object(taskctl, "request", side_effect=OSError("transport lost after submission")) as request:
            with self.assertRaises(OSError):
                client.api("", "POST", {"id": "keep-id", "shell": "true"})
            self.assertEqual(request.call_count, 1)

    def test_default_and_named_directories_are_stable_and_validate_names(self):
        self.assertEqual(taskctl.service_dir(None, "canary"), taskctl.service_dir(None, "default") / "instances/canary")
        for name in ("../default", "", "a/b"):
            with self.assertRaises(ValueError):
                taskctl.service_dir(None, name)


if __name__ == "__main__":
    unittest.main()
