"""Read-only ranking clients: capability, fencing, filters and malformed data."""
import argparse
import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest import mock
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'skills/simplehtmlwatch-netdata/scripts'))
import metricsctl
import service_client

spec = importlib.util.spec_from_file_location('idle_taskctl', ROOT / 'skills/cluster-task-controller/scripts/taskctl.py')
taskctl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(taskctl)


def args(**values):
    result = dict(action='idle-ranking', instance='test', url=None, data_dir=None,
                  service_id=None, expect_version=None, require_capability=[], allow_legacy=False,
                  group='A5 B02', model='Ascend950DT', devices_per_machine=16,
                  min_free_hbm_mb=32768, max_sample_age=90)
    result.update(values)
    return argparse.Namespace(**result)


class IdleRankingClients(unittest.TestCase):
    def test_metrics_requires_ranking_without_netdata(self):
        client = service_client.Client(args())
        self.assertEqual(client.required, {'idle-machine-ranking-v1'})

    def test_metrics_filters_zero_and_failures(self):
        fake = mock.Mock()
        fake.api.return_value = dict(schema='simplehtmlwatch.idle-ranking.v1', machines=[], requiresLiveCheck=True)
        result, code = metricsctl.run(args(min_free_hbm_mb=0), fake)
        self.assertEqual(code, 0)  # No candidates is not a service failure.
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(fake.api.call_args.args[0]).query)
        self.assertEqual(query['minFreeHbmMB'], ['0'])
        self.assertEqual(query['group'], ['A5 B02'])
        fake.api.return_value['historyStorageError'] = 'write failed'
        self.assertEqual(metricsctl.run(args(), fake)[1], 3)
        fake.api.return_value['requiresLiveCheck'] = False
        with self.assertRaises(ValueError):
            metricsctl.run(args(), fake)

    def test_bad_resources_rejected_before_network(self):
        for values in (dict(devices_per_machine=0), dict(min_free_hbm_mb=-1), dict(max_sample_age=301)):
            fake = mock.Mock()
            with self.assertRaises(ValueError):
                metricsctl.run(args(**values), fake)
            fake.api.assert_not_called()
            client = taskctl.Client(args(**values))
            with mock.patch.object(client, 'prepare') as prepare:
                with self.assertRaises(ValueError):
                    client.idle_ranking(args(**values))
                prepare.assert_not_called()

    def test_taskctl_get_is_bound_to_run_and_never_reserves(self):
        client = taskctl.Client(args())
        client.url, client.run_id, client.token = 'http://127.0.0.1:12345', 'run', 'session'
        payload = dict(schema='simplehtmlwatch.idle-ranking.v1', machines=[], requiresLiveCheck=True)
        with mock.patch.object(client, 'prepare'), mock.patch.object(taskctl, 'request', return_value=json.dumps(payload)) as request:
            self.assertEqual(client.idle_ranking(args()), payload)
        self.assertIn('idle-machine-ranking-v1', client.capabilities)
        self.assertEqual(request.call_count, 1)
        self.assertEqual(request.call_args.kwargs, {'run_id': 'run'})
        self.assertIn('/api/idle-ranking?', request.call_args.args[0])
        self.assertNotIn('session', request.call_args.args[0])

    def test_legacy_and_uncertain_read_never_replay(self):
        client = taskctl.Client(args(url='http://127.0.0.1:12345', allow_legacy=True))
        with self.assertRaises(ValueError):
            client.idle_ranking(args())
        client = taskctl.Client(args())
        client.url, client.run_id, client.token = 'http://127.0.0.1:12345', 'run', 'session'
        with mock.patch.object(client, 'prepare'), mock.patch.object(taskctl, 'request', side_effect=TimeoutError()) as request:
            with self.assertRaises(TimeoutError):
                client.idle_ranking(args())
            self.assertEqual(request.call_count, 1)


if __name__ == '__main__':
    unittest.main()
