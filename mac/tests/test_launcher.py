import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import launcher


class LauncherTests(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[2] / '.build/test-tmp'
        root.mkdir(parents=True, exist_ok=True)
        temporary = tempfile.TemporaryDirectory(dir=root)
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name) / 'Fengxin-Mac'
        self.base.mkdir()
        self.config = self.base / 'config.json'
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(launcher, 'BASE', self.base))
        self.stack.enter_context(patch.object(launcher, 'CONFIG', self.config))
        self.argv = ['launcher.py', 'start', '--desktop', '--no-open']
        self.stack.enter_context(patch.object(sys, 'argv', self.argv))
        self.spawn = self.stack.enter_context(patch('launcher.subprocess.Popen'))
        self.open_request = self.stack.enter_context(patch('launcher.urllib.request.build_opener')).return_value.open
        self.connect = self.stack.enter_context(patch('launcher.socket.create_connection'))

    def test_first_launch_stops_at_occupied_port(self):
        with self.assertRaisesRegex(RuntimeError, '占用'):
            launcher.main()
        self.open_request.assert_not_called()
        self.spawn.assert_not_called()
        self.assertFalse(self.config.exists())

    def test_existing_service_with_other_credentials_is_not_restarted(self):
        original = json.dumps({'web_listen': '127.0.0.1:7576', 'web_password': 'fixture'})
        self.config.write_text(original)
        self.open_request.side_effect = urllib.error.HTTPError('http://fixture', 401, 'Unauthorized', {}, None)
        with self.assertRaisesRegex(RuntimeError, '凭据'):
            launcher.main()
        self.spawn.assert_not_called()
        self.assertEqual(self.config.read_text(), original)
        self.assertFalse((self.base / 'data/recovery-session.json').exists())

    def test_first_launch_does_not_claim_an_unresponsive_port(self):
        self.connect.side_effect = TimeoutError()
        with self.assertRaisesRegex(RuntimeError, '暂时无法确认'):
            launcher.main()
        self.spawn.assert_not_called()
        self.assertFalse(self.config.exists())

    def test_stop_rejects_wrong_service_before_shutdown(self):
        self.argv[1] = 'stop'
        self.config.write_text(json.dumps({'web_listen': '127.0.0.1:7576', 'web_password': 'fixture'}))
        self.open_request.return_value.__enter__.return_value.read.return_value = b'{"version":"other","modem":{}}'
        with self.assertRaisesRegex(RuntimeError, '不匹配'):
            launcher.main()
        self.open_request.assert_called_once()
        self.assertEqual(self.open_request.call_args.args[0].get_method(), 'GET')

    def test_managed_stop_during_reconnect_uses_the_owned_session(self):
        self.argv[1] = 'stop'
        self.config.write_text(json.dumps({'web_listen': '127.0.0.1:7576', 'web_password': 'fixture'}))
        (self.base / 'data').mkdir()
        session = self.base / 'data/recovery-session.json'
        session.write_text(json.dumps({'session': 'fixture', 'desired_running': True}))
        self.open_request.side_effect = urllib.error.URLError(ConnectionRefusedError())
        with patch('launcher.managed_active', side_effect=[True, False]), contextlib.redirect_stdout(io.StringIO()):
            launcher.main()
        self.assertEqual(json.loads(session.read_text()), {'session': 'fixture', 'desired_running': False})
        self.open_request.assert_called_once()
        self.assertEqual(self.open_request.call_args.args[0].get_method(), 'GET')
        self.spawn.assert_not_called()

    def test_desktop_starts_once_after_connection_refused_even_without_modem(self):
        offline = {'version': '1.0.0-fengxin', 'modem': {'connected': False}, 'desktop': True}
        reply = self.open_request.return_value
        reply.__enter__.return_value.read.return_value = json.dumps(offline).encode()
        self.connect.side_effect = ConnectionRefusedError()
        self.spawn.return_value.poll.return_value = None
        self.spawn.return_value.pid = 1234
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            launcher.main()
        self.spawn.assert_called_once()
        self.assertIn('device_supervisor.py', self.spawn.call_args.args[0][1])
        self.assertEqual(json.loads(output.getvalue())['modem'], offline['modem'])
        self.assertTrue(json.loads(self.config.read_text())['desktop'])
        self.open_request.assert_called_once()
        self.assertEqual(self.open_request.call_args.args[0].get_method(), 'GET')
