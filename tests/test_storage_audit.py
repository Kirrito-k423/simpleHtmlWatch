import datetime as dt
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("storage_audit", Path(__file__).resolve().parents[1] / "scripts/storage_audit.py")
audit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit)


class StorageAuditTests(unittest.TestCase):
    def test_read_only_measurement_and_external_bounded_history(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "data"
            root.mkdir()
            content = root / "evidence.txt"
            content.write_text("original result")
            state = Path(folder) / "audit.json"
            now = dt.datetime(2026, 10, 20, tzinfo=dt.timezone.utc)
            first = audit.run(root, state, True, now)
            self.assertEqual(first["growthBytes"], 0)
            baseline = json.loads(state.read_text())["baseline"]
            with self.assertRaises(FileExistsError):
                audit.run(root, state, True, now)
            for day in range(40):
                result = audit.run(root, state, now=now + dt.timedelta(days=day))
                audit.run(root, state, now=now + dt.timedelta(days=day, minutes=1))
            saved = json.loads(state.read_text())
            self.assertEqual(saved["baseline"], baseline)
            self.assertEqual(len(saved["samples"]), 35)
            self.assertLess(state.stat().st_size, 128 * 1024)
            self.assertEqual(result["status"], "within_target")
            self.assertEqual(content.read_text(), "original result")
            self.assertEqual(list(root.iterdir()), [content])
            with self.assertRaises(ValueError):
                audit.run(root, root / "bad.json", True, now)

    def test_peak_breach_remains_a_failure_after_file_removed(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "data"
            root.mkdir()
            state = Path(folder) / "audit.json"
            now = dt.datetime(2026, 10, 20, tzinfo=dt.timezone.utc)
            audit.run(root, state, True, now)
            payload = root / "large-file"
            payload.write_bytes(b"x" * 6_000_000)
            self.assertEqual(audit.run(root, state, now=now + dt.timedelta(days=1))["status"], "exceeded")
            payload.unlink()
            self.assertEqual(audit.run(root, state, now=now + dt.timedelta(days=1, minutes=1))["status"], "exceeded")
            self.assertEqual(audit.run(root, state, now=now + dt.timedelta(days=31))["status"], "exceeded")

    def test_elapsed_time_alone_does_not_prove_thirty_day_coverage(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "data"
            root.mkdir()
            state = Path(folder) / "audit.json"
            now = dt.datetime(2026, 10, 20, tzinfo=dt.timezone.utc)
            audit.run(root, state, True, now)
            self.assertEqual(audit.run(root, state, now=now + dt.timedelta(days=30))["status"], "insufficient_coverage")


if __name__ == "__main__":
    unittest.main()
