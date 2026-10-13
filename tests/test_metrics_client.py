"""指标 Skill 的实例校验、缺失值与批量部分失败回归。"""
import argparse
import copy
import json
from pathlib import Path
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parents[1] / "skills/simplehtmlwatch-netdata/scripts"
sys.path.insert(0, str(SCRIPTS))
import metricsctl
import service_client


def snapshot(error=False):
    charts = [{"context": context, "name": context, "unit": "%", "lastEntry": 30,
               "series": [{"name": "dimension", "points": [{"at": 20, "value": None}, {"at": 10, "value": 0}]}]}
              for context in ("system.cpu", "system.ram", "system.io", "system.net")]
    if error:
        charts[1] = {"context": "system.ram", "error": "采集器无数据", "series": []}
    return {"machineId": "m1", "source": "netdata", "charts": charts}


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.requests = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.requests.append((self.path, dict(self.headers)))
                code = 200
                if self.path == "/healthz":
                    data = json.dumps(owner.live).encode()
                elif self.path == "/":
                    data = b'<meta name="watch-token" content="test-session">'
                elif self.path == "/large":
                    data = b'x' * 100
                elif self.headers.get("X-Watch-Instance") != owner.live["runId"]:
                    code, data = 409, json.dumps({"error": "实例已变化"}).encode()
                else:
                    data = json.dumps(snapshot()).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *_):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:" + str(self.server.server_port)
        self.live = dict(schema="simplehtmlwatch.service.v1", apiVersion=1, instance="test", serviceId="service", runId="run1", buildId="build", version="v1", url=self.url, dataDir=self.temp.name, capabilities=["netdata-metrics-v1"])
        self.registry = Path(self.temp.name) / "service.json"
        self.registry.write_text(json.dumps(self.live))
        args = argparse.Namespace(url=None, data_dir=self.temp.name, instance="test", service_id=None, expect_version=None, require_capability=[], action="query")
        self.client = service_client.Client(args)

    def test_api_binds_run_and_hides_session_from_result(self):
        result = self.client.api("/api/metrics?machineId=m1")
        headers = self.requests[-1][1]
        self.assertEqual(headers["X-Watch-Instance"], "run1")
        self.assertEqual(headers["X-Watch-Token"], "test-session")
        self.assertNotIn("test-session", json.dumps(result))

    def test_wrong_run_or_service_rejected_before_session(self):
        for field in ("runId", "serviceId", "buildId", "dataDir", "version", "instance"):
            with self.subTest(field=field):
                saved = self.live[field]
                self.live[field] = "wrong"
                with self.assertRaises(RuntimeError):
                    self.client.api("/api/metrics?machineId=m1")
                self.live[field] = saved
        self.assertTrue(all(p == "/healthz" for p, _ in self.requests))

    def test_missing_capability_and_nonlocal_url_rejected(self):
        self.live["capabilities"] = []
        self.registry.write_text(json.dumps(self.live))
        with self.assertRaisesRegex(RuntimeError, "缺少能力"):
            self.client.discover()
        for url in ("https://127.0.0.1:1", "http://example.com:1", "http://user:pass@localhost:1", "http://localhost:1/path"):
            with self.assertRaises(ValueError):
                service_client.base_url(url)

    def test_restart_during_request_cannot_read_other_run(self):
        self.client.api("/api/metrics?machineId=m1")
        old_discover = self.client.discover
        def discovery_then_restart():
            info = copy.deepcopy(old_discover())
            self.live["runId"] = "run2"
            return info
        with patch.object(self.client, "discover", discovery_then_restart):
            with self.assertRaisesRegex(RuntimeError, "409"):
                self.client.api("/api/metrics?machineId=m1")

    def test_oversize_response_not_silently_truncated(self):
        with self.assertRaisesRegex(ValueError, "上限"):
            service_client.request(self.url + "/large", limit=10)

    def test_write_or_arbitrary_paths_rejected(self):
        for path in ("/api/trust", "/api/refresh", "/api/tasks/submit", "/api/config/other"):
            with self.assertRaises(ValueError):
                self.client.api(path)
        self.assertEqual(self.requests, [])

    def test_utilization_capability_is_independent_of_netdata(self):
        args = argparse.Namespace(url=None, data_dir=self.temp.name, instance="test", service_id=None, expect_version=None, require_capability=[], action="utilization")
        self.client = service_client.Client(args)
        self.assertEqual(self.client.required, {"accelerator-utilization-v1", "accelerator-occupancy-v1"})
        self.live["capabilities"] = ["accelerator-utilization-v1", "accelerator-occupancy-v1"]
        self.registry.write_text(json.dumps(self.live))
        self.client.api("/api/utilization?period=yesterday")
        self.assertEqual(self.requests[-1][0], "/api/utilization?period=yesterday")
        self.live["capabilities"] = ["accelerator-utilization-v1"]
        self.registry.write_text(json.dumps(self.live))
        with self.assertRaisesRegex(RuntimeError, "accelerator-occupancy-v1"):
            self.client.discover()
        self.live["capabilities"] = ["netdata-metrics-v1"]
        self.registry.write_text(json.dumps(self.live))
        with self.assertRaisesRegex(RuntimeError, "缺少能力"):
            self.client.discover()


class SummaryTests(unittest.TestCase):
    def test_utilization_missing_history_has_exit_three_without_zero_fill(self):
        fake = unittest.mock.Mock()
        fake.api.return_value = {"source": "npu-smi", "machines": [{"id": "m1", "mean": None, "coveragePercent": 0, "hours": [{"mean": None}]}]}
        args = argparse.Namespace(action="utilization", period="yesterday", date=None, days=None, machine=None, group=None)
        result, code = metricsctl.run(args, fake)
        self.assertEqual(code, 3)
        self.assertIsNone(result["machines"][0]["hours"][0]["mean"])
        self.assertFalse(result["complete"])
        fake.api.return_value = {"source": "npu-smi", "machines": [{"mean": 0, "coveragePercent": 100, "occupiedPercent": None, "occupancyCoveragePercent": 0}]}
        result, code = metricsctl.run(args, fake)
        self.assertEqual(code, 3)
        self.assertTrue(result["utilizationComplete"])
        self.assertFalse(result["occupancyComplete"])
        fake.api.return_value["machines"][0].update(occupiedPercent=0, occupancyCoveragePercent=100)
        result, code = metricsctl.run(args, fake)
        self.assertEqual(code, 0)
        fake.api.return_value["storageError"] = "disk full"
        self.assertEqual(metricsctl.run(args, fake)[1], 3)

    def test_utilization_query_validates_dates_and_scope(self):
        args = argparse.Namespace(period=None, date="2026-10-08", days=7, machine="m1", group="A5 B01")
        path = metricsctl.utilization_path(args)
        self.assertIn("date=2026-10-08", path)
        self.assertIn("machineId=m1", path)
        for changes in ({"date": "2026-99-08"}, {"days": 0}, {"days": 31}, {"period": "week"}, {"machine": "m1&x=1"}, {"date": None}):
            changed = argparse.Namespace(**(vars(args) | changes))
            with self.assertRaises(ValueError):
                metricsctl.utilization_path(changed)
    def test_null_and_zero_are_distinct_and_latest_uses_timestamp(self):
        result, partial = metricsctl.summarize(snapshot())
        self.assertFalse(partial)
        series = result["charts"][0]["series"][0]
        self.assertEqual(series["valuePoints"], 1)
        self.assertEqual(series["missingPoints"], 1)
        self.assertEqual(series["lastInWindow"], {"at": 10, "value": 0})
        self.assertEqual(result["charts"][0]["lastEntry"], 30)

    def test_partial_200_response_has_exit_three_even_raw(self):
        fake = unittest.mock.Mock()
        fake.api.return_value = snapshot(error=True)
        args = argparse.Namespace(action="query", machine="m1", task=None, after=None, before=None, raw=True)
        result, code = metricsctl.run(args, fake)
        self.assertEqual(code, 3)
        self.assertIn("error", result["charts"][1])

    def test_machine_list_excludes_profile_and_agent_secrets(self):
        fake = unittest.mock.Mock()
        fake.api.side_effect = [{"config": {"profiles": [{"password": "secret"}], "machines": [{"id": "m1", "netdata": {"token": "secret", "enabled": True}}]}}, {"m1": {"status": "online", "results": ["secret"]}}]
        self.assertNotIn("secret", json.dumps(metricsctl.machines(fake)))

    def test_batch_reports_skipped_and_continues_after_failure(self):
        rows = [{"id": name, "name": name, "group": "A3", "enabled": True, "netdata": {"enabled": name != "off"}, "ssh": {}} for name in ("bad", "good", "off")]
        fake = unittest.mock.Mock()
        fake.api.side_effect = [RuntimeError("HTTP 429"), snapshot()]
        args = argparse.Namespace(action="summary", group="A3", after=None, before=None, raw=False)
        with patch.object(metricsctl, "machines", return_value=rows):
            result, code = metricsctl.run(args, fake)
        self.assertEqual(code, 3)
        self.assertEqual((result["queried"], result["succeeded"], result["failed"]), (2, 1, 1))
        self.assertEqual(result["skipped"][0]["id"], "off")
        self.assertEqual(fake.api.call_count, 2)

    def test_query_window_and_selector_rejected(self):
        for kwargs in ({"machine": "bad&id=x"}, {"machine": "m", "task": "t"}, {"machine": "m", "after": 1}, {"machine": "m", "after": 1, "before": 8*86400}):
            with self.assertRaises(ValueError):
                metricsctl.query_path(**kwargs)


if __name__ == "__main__":
    unittest.main()
