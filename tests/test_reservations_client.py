"""Fixed-ID reservation client tests. No real SSH machines or live service writes."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("reservations_client", ROOT / "skills/cluster-task-controller/scripts/taskctl.py")
client = importlib.util.module_from_spec(spec)
spec.loader.exec_module(client)


class ReservationClientTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.payload = {"id": "fixed-reservation", "machineId": "node-one", "machineIds": ["node-one", "node-two"],
                        "taskId": "fixed-task", "planSha256": "a" * 64}
        self.path = Path(self.temp.name) / "request.json"
        self.path.write_text(json.dumps(self.payload))
        self.fake = mock.Mock()
        self.fake.capabilities = set()
        self.fake.api.return_value = {"reservations": {"fixed-reservation": self.payload}}

    def run_cli(self, args):
        with mock.patch.object(sys, "argv", ["taskctl.py", *args]), mock.patch.object(client, "Client", return_value=self.fake), contextlib.redirect_stdout(io.StringIO()):
            return client.main()

    def test_fixed_group_request_and_capabilities(self):
        self.assertEqual(self.run_cli(["reserve", "--request", str(self.path)]), 0)
        self.fake.api.assert_called_once_with("/reservations", "POST", self.payload)
        self.assertEqual(self.fake.capabilities, {"reservations", "atomic-group-reservations-v1", "forwarded-resource-identity-v1"})

    def test_uncertain_write_is_not_replayed(self):
        self.fake.api.side_effect = TimeoutError("receipt lost")
        with self.assertRaises(TimeoutError):
            self.run_cli(["reserve", "--request", str(self.path)])
        self.fake.api.assert_called_once_with("/reservations", "POST", self.payload)

    def test_missing_fixed_id_and_legacy_never_write(self):
        del self.payload["id"]
        self.path.write_text(json.dumps(self.payload))
        for args in (["reserve", "--request", str(self.path)],
                     ["--url", "http://127.0.0.1:1234", "--allow-legacy", "release-reservation", "fixed-reservation"]):
            with self.assertRaises(ValueError):
                self.run_cli(args)
            self.fake.api.assert_not_called()

    def test_query_original_id_and_release(self):
        self.run_cli(["reservations", "--id", "fixed-reservation"])
        self.fake.api.assert_called_once_with("/reservations")
        self.fake.api.reset_mock()
        self.run_cli(["release-reservation", "fixed-reservation"])
        self.fake.api.assert_called_once_with("/reservations/release", "POST", {"id": "fixed-reservation"})

    def test_frozen_submit_passes_grant_and_original_task(self):
        self.run_cli(["submit", "--id", "fixed-task", "--machine", "node-one", "--command", "echo frozen",
                      "--reservation", "fixed-reservation", "--reservation-grant", "fixed-grant", "--reservation-generation", "1"])
        self.fake.api.assert_called_once_with("", "POST", {"id": "fixed-task", "shell": "echo frozen", "machineId": "node-one",
                                                       "reservationId": "fixed-reservation", "reservationGrantId": "fixed-grant", "reservationGeneration": 1})

    def test_incomplete_grant_or_selector_never_submits(self):
        common = ["submit", "--command", "echo frozen", "--reservation", "fixed-reservation"]
        for rest in ([], ["--id", "fixed-task"], ["--id", "fixed-task", "--machine", "node-one", "--reservation-grant", "grant-only"]):
            with self.assertRaises(ValueError):
                self.run_cli(common + rest)
            self.fake.api.assert_not_called()


if __name__ == "__main__":
    unittest.main()
