import json
from pathlib import Path
import signal
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from device_supervisor import RecoveryPolicy, Supervisor, private_write


class FakeCore:
    def __init__(self, pid):
        self.pid = pid
        self.returncode = None
        self.signals = []

    def poll(self):
        return self.returncode

    def send_signal(self, value):
        self.signals.append(value)
        self.returncode = 0


class FakeSupervisor(Supervisor):
    def __init__(self, base):
        super().__init__(base, 'fixture-session', {'web_listen': '127.0.0.1:7576', 'web_password': 'fixture-only'})
        self.node = 'usb-1'
        self.status = {'connected': True, 'registered': False}
        self.free = True
        self.processes = []
        self.events = []

    def epoch(self):
        return self.node

    def probe(self):
        return self.status

    def device_free(self):
        return self.free

    def spawn(self):
        process = FakeCore(1000 + len(self.processes))
        self.processes.append(process)
        return process

    def event(self, kind, **values):
        self.events.append((kind, values))


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[2] / '.build/test-tmp'
        root.mkdir(parents=True, exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(prefix='case-', dir=root)
        self.base = Path(self.directory.name)
        (self.base / 'data').mkdir()
        self.save_session(True)
        self.runner = FakeSupervisor(self.base)
        self.assertTrue(self.runner.tick(0))

    def tearDown(self):
        self.directory.cleanup()

    def save_session(self, desired, session='fixture-session'):
        private_write(self.base / 'data/recovery-session.json', json.dumps({'session': session, 'desired_running': desired}))

    def begin_loss(self):
        self.runner.status = {'connected': False}
        self.runner.node = 'usb-2'
        self.assertTrue(self.runner.tick(16))
        self.assertTrue(self.runner.tick(22))

    def test_reenumeration_gracefully_reopens_once(self):
        self.begin_loss()
        self.assertEqual(self.runner.processes[0].signals, [signal.SIGTERM])
        self.assertEqual(len(self.runner.processes), 1)
        self.assertTrue(self.runner.tick(23))
        self.assertEqual(len(self.runner.processes), 2)
        self.assertEqual([kind for kind, _ in self.runner.events].count('usb_reconnect_requested'), 1)

    def test_stop_between_disconnect_and_reopen_prevents_new_process(self):
        self.begin_loss()
        self.save_session(False)
        self.assertFalse(self.runner.tick(23))
        self.assertEqual(len(self.runner.processes), 1)

    def test_obsolete_session_cannot_reopen(self):
        self.begin_loss()
        self.save_session(True, 'a-new-owner')
        self.assertFalse(self.runner.tick(23))
        self.assertEqual(len(self.runner.processes), 1)

    def test_searching_or_no_signal_is_not_a_reconnect_trigger(self):
        for now in [16, 30, 60, 180]:
            self.runner.node = 'another-object-' + str(now)
            self.assertTrue(self.runner.tick(now))
        self.assertEqual(len(self.runner.processes), 1)
        self.assertEqual(self.runner.processes[0].signals, [])

    def test_absent_device_keeps_offline_service_running(self):
        self.runner.node = None
        self.runner.status = {'connected': False}
        for now in [16, 30, 180]:
            self.assertTrue(self.runner.tick(now))
        self.assertEqual(self.runner.processes[0].signals, [])

    def test_external_cli_lock_is_not_interrupted(self):
        self.runner.free = False
        self.begin_loss()
        self.assertEqual(self.runner.processes[0].signals, [])
        self.runner.free = True
        self.assertTrue(self.runner.tick(30))
        self.assertEqual(self.runner.processes[0].signals, [signal.SIGTERM])

    def test_unexpected_core_exit_is_not_replayed(self):
        self.runner.processes[0].returncode = 1
        self.assertFalse(self.runner.tick(20))
        self.assertEqual(len(self.runner.processes), 1)

    def test_at_timeout_allows_only_one_same_object_reopen(self):
        policy = RecoveryPolicy()
        policy.attempted('usb-1', 0, initial=True)
        offline = {'connected': False}
        self.assertFalse(policy.should_reconnect('usb-1', offline, 10, True))
        self.assertTrue(policy.should_reconnect('usb-1', offline, 16, True))
        policy.attempted('usb-1', 16)
        self.assertFalse(policy.should_reconnect('usb-1', offline, 30, True))
        self.assertFalse(policy.should_reconnect('usb-1', offline, 100, True))
        self.assertTrue(policy.should_reconnect('usb-2', offline, 101, True))

    def test_status_probe_never_posts_business_requests(self):
        class Response:
            def __enter__(self): return self
            def __exit__(self, *_): pass
            def read(self):
                return json.dumps({'version': '1.0.0-fengxin', 'desktop': True,
                                   'modem': {'port': 'usb:2c7c:0125', 'connected': False}}).encode()
        requests = []
        class Opener:
            def open(self, request, timeout):
                requests.append(request)
                return Response()
        with patch('device_supervisor.urllib.request.build_opener', return_value=Opener()):
            result = Supervisor.probe(self.runner)
        self.assertEqual(result, {'connected': False})
        self.assertEqual(len(requests), 1)
        self.assertEqual(requests[0].full_url, 'http://127.0.0.1:7576/api/status')
        self.assertEqual(requests[0].get_method(), 'GET')
        self.assertIsNone(requests[0].data)


if __name__ == '__main__':
    unittest.main()
