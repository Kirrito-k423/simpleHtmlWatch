"""结果包分块下载、容量边界及失败清理回归。"""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
from pathlib import Path
import tempfile
import sys
import threading
import unittest
from unittest.mock import patch

CLIENT = Path(__file__).resolve().parents[1] / "skills/cluster-task-controller/scripts/taskctl.py"
spec = importlib.util.spec_from_file_location("archive_taskctl", CLIENT)
taskctl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(taskctl)


class Response(io.BytesIO):
    def __init__(self, data, length=None):
        super().__init__(data)
        self.headers = {} if length is None else {"Content-Length": str(length)}

    def read(self, size=-1):
        if size < 0 or size > 65536:
            raise AssertionError("must read bounded chunks")
        return super().read(size)


class ArchiveDownloadTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "完整结果.tar.gz"

    def test_actual_http_transfer_over_old_limit(self):
        chunk = bytes(range(256)) * 256
        expected = hashlib.sha256()
        for _ in range(96):
            expected.update(chunk)

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(len(chunk) * 96))
                self.end_headers()
                for _ in range(96):
                    self.wfile.write(chunk)

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            response = taskctl.request(f"http://127.0.0.1:{server.server_port}/archive", binary=True)
            self.assertEqual(taskctl.save_archive(response, self.path), 6 * 1024 * 1024)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        with self.path.open("rb") as saved:
            self.assertEqual(hashlib.file_digest(saved, "sha256").digest(), expected.digest())

    def test_ten_gib_header_overflow_rejected_without_creating_output(self):
        self.assertEqual(taskctl.MAX_TASK_ARCHIVE, 10737418240)
        response = Response(b"", taskctl.MAX_TASK_ARCHIVE + 1)
        with self.assertRaisesRegex(RuntimeError, "10 GiB"):
            taskctl.save_archive(response, self.path)
        self.assertTrue(response.closed)
        self.assertFalse(self.path.exists())

    def test_exact_limit_allowed_and_one_byte_over_rejected_without_length(self):
        # Scale the boundary to avoid allocating 10 GiB in every CI runner.
        with patch.object(taskctl, "MAX_TASK_ARCHIVE", 8):
            self.assertEqual(taskctl.save_archive(Response(b"12345678"), self.path), 8)
            self.path.unlink()
            with self.assertRaisesRegex(RuntimeError, "10 GiB"):
                taskctl.save_archive(Response(b"123456789"), self.path)
        self.assertFalse(self.path.exists())

    def test_truncated_download_removed(self):
        with self.assertRaisesRegex(RuntimeError, "下载不完整"):
            taskctl.save_archive(Response(b"partial", 20), self.path)
        self.assertFalse(self.path.exists())

    def test_empty_download_removed(self):
        with self.assertRaisesRegex(RuntimeError, "为空"):
            taskctl.save_archive(Response(b"", 0), self.path)
        self.assertFalse(self.path.exists())

    def test_existing_file_preserved_and_response_closed(self):
        self.path.write_bytes(b"keep")
        response = Response(b"replacement")
        with self.assertRaises(FileExistsError):
            taskctl.save_archive(response, self.path)
        self.assertEqual(self.path.read_bytes(), b"keep")
        self.assertTrue(response.closed)

    def test_network_failure_and_interrupt_remove_partial_file(self):
        for failure in (OSError("connection lost"), KeyboardInterrupt()):
            class Interrupted(Response):
                def read(self, size=-1):
                    if self.tell():
                        raise failure
                    return super().read(size)
            response = Interrupted(b"partial", 20)
            with self.assertRaises(type(failure)):
                taskctl.save_archive(response, self.path)
            self.assertFalse(self.path.exists())
            self.assertTrue(response.closed)

    def test_collection_can_wait_beyond_ordinary_api_timeout(self):
        args = argparse.Namespace(url=None, data_dir=None, instance="default", allow_legacy=False,
                                  expect_version=None, require_capability=[], action="collect", service_id=None)
        client = taskctl.Client(args)
        client.url, client.token, client.run_id = "http://127.0.0.1:1234", "token", "run"
        with patch.object(client, "prepare"), patch.object(taskctl, "request", return_value="{}") as request:
            client.api("/collect?id=example", "POST", timeout=None)
            self.assertIsNone(request.call_args.args[-1])
            client.api("?id=example")
            self.assertEqual(request.call_args.args[-1], 45)

    def test_wait_does_not_finish_while_large_archive_is_still_collecting(self):
        finished = {"status": "succeeded", "finishedAt": "2026-09-29T00:00:00Z", "exitCode": 0, "archiveReady": False}
        with patch.object(sys, "argv", ["taskctl.py", "wait", "example"]), patch.object(taskctl, "Client") as client, patch.object(taskctl.time, "sleep") as sleep, patch.object(taskctl, "show"):
            client.return_value.api.side_effect = [finished, {**finished, "archiveReady": True}]
            self.assertEqual(taskctl.main(), 0)
            self.assertEqual(client.return_value.api.call_count, 2)
            sleep.assert_called_once()


if __name__ == "__main__":
    unittest.main()
