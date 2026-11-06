#!/usr/bin/env python3
"""Recover a desktop gateway after USB loss, without retrying business requests."""
import base64
import fcntl
import http.client
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import urllib.request
from datetime import datetime

os.umask(0o077)


def private_write(path, text):
    staging = path.with_suffix(path.suffix + '.new')
    staging.write_text(text, encoding='utf-8')
    staging.chmod(0o600)
    staging.replace(path)


def read_session(base):
    try:
        value = json.loads((base / 'data' / 'recovery-session.json').read_text())
        return value if isinstance(value, dict) else {}
    except (OSError, ValueError):
        return {}


def managed_active(base):
    with (base / 'data' / 'recovery.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return False
        except BlockingIOError:
            return True


def usb_epoch():
    tree = subprocess.run(['/usr/sbin/ioreg', '-p', 'IOUSB', '-w', '0'],
                          capture_output=True, text=True, timeout=3, check=True).stdout
    matches = re.findall(r'EC20-CE-HDLG@[^\n]*<class IOUSBHostDevice, id (0x[0-9a-f]+),', tree)
    return matches[0] if len(matches) == 1 else None


class RecoveryPolicy:
    def __init__(self, cooldown=15, quiet_period=5):
        self.cooldown = cooldown
        self.quiet_period = quiet_period
        self.attempted_epoch = None
        self.last_attempt = float('-inf')
        self.same_epoch_retry_used = False
        self.offline_since = None

    def attempted(self, epoch, now, initial=False):
        self.same_epoch_retry_used = not initial and epoch == self.attempted_epoch
        self.attempted_epoch = epoch
        self.last_attempt = now
        self.offline_since = None

    def should_reconnect(self, epoch, status, now):
        # No restart for searching/no signal while the control channel works.
        if status is None:
            return False
        if status['connected']:
            self.offline_since = None
            return False
        if self.offline_since is None:
            self.offline_since = now
        if epoch is None:
            return False
        if now - self.last_attempt < self.cooldown or now - self.offline_since < self.quiet_period:
            return False
        return epoch != self.attempted_epoch or not self.same_epoch_retry_used


class Supervisor:
    probe_interval = 5

    def __init__(self, base, session, cfg):
        self.base = Path(base)
        self.session = session
        self.cfg = cfg
        self.core = None
        self.retiring = None
        self.stop_requested = False
        self.policy = RecoveryPolicy()
        self.next_probe_at = 0

    def desired(self):
        current = read_session(self.base)
        return (not self.stop_requested and current.get('session') == self.session
                and current.get('desired_running') is True)

    def event(self, kind, **values):
        row = {'time': datetime.now().astimezone().isoformat(timespec='seconds'),
               'kind': kind, **values}
        with (self.base / 'logs' / 'recovery-events.jsonl').open('a', encoding='utf-8') as log:
            log.write(json.dumps(row, ensure_ascii=False) + '\n')
            log.flush()

    def epoch(self):
        try:
            return usb_epoch()
        except (OSError, subprocess.SubprocessError):
            return None

    def device_free(self):
        shared = self.base.parent / 'EC20-Mac' / 'data' / 'modem.lock'
        try:
            with shared.open('a') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return True
        except (OSError, BlockingIOError):
            return False

    def probe(self):
        # Only authenticated, loopback reads; no SMS/call/network action routes.
        address = 'http://' + self.cfg['web_listen']
        auth = base64.b64encode(('admin:' + self.cfg['web_password']).encode()).decode()
        request = urllib.request.Request(address + '/api/status', headers={'Authorization': 'Basic ' + auth})
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(request, timeout=2) as response:
                value = json.loads(response.read())
            if not isinstance(value, dict):
                return None
            modem = value.get('modem')
            if (value.get('version') != '1.0.0-fengxin' or value.get('desktop') is not True
                    or not isinstance(modem, dict) or modem.get('port') != 'usb:2c7c:0125'
                    or not isinstance(modem.get('connected'), bool)):
                return None
            return {'connected': modem['connected']}
        except (OSError, ValueError, http.client.HTTPException):
            return None

    def spawn(self):
        return subprocess.Popen([str(self.base / 'fengxin-core'), '-config', str(self.base / 'config.json')],
                                cwd=self.base, stdin=subprocess.DEVNULL)

    def start_core(self, epoch, now, initial=False):
        if not self.desired():
            return False
        self.core = self.spawn()
        private_write(self.base / 'data' / 'gateway.pid', str(self.core.pid) + '\n')
        self.policy.attempted(epoch, now, initial)
        self.retiring = None
        self.event('gateway_started', pid=self.core.pid, usb_epoch=epoch)
        return True

    def retire_core(self, reason):
        if self.retiring is not None:
            return
        self.retiring = reason
        self.event('gateway_stopping', reason=reason)
        # The owned Go child handles SIGTERM with its normal graceful shutdown.
        # Do not kill an arbitrary PID or replay any pending command.
        if self.core.poll() is None:
            try:
                self.core.send_signal(signal.SIGTERM)
            except ProcessLookupError:
                pass

    def tick(self, now):
        if not self.desired():
            if self.core is not None and self.core.poll() is None:
                self.retire_core('stop_requested')
                return True
            return False
        if self.core is None:
            return self.start_core(self.epoch(), now, initial=True)
        if self.retiring is not None:
            if self.core.poll() is None:
                return True
            return self.start_core(self.epoch(), now)
        if self.core.poll() is not None:
            self.event('gateway_exited', code=self.core.returncode)
            return False  # User/API exit and unrelated crashes are not restarted.
        # Keep checking process exit and explicit stop every tick. Hardware
        # discovery and the shared device lock are only needed for recovery.
        if now < self.next_probe_at:
            return True
        self.next_probe_at = now + self.probe_interval
        status = self.probe()
        epoch = self.epoch() if status is not None and not status['connected'] else None
        if self.policy.should_reconnect(epoch, status, now) and self.device_free():
            if self.desired():
                self.event('usb_reconnect_requested', usb_epoch=epoch)
                self.retire_core('usb_reconnect')
        return True

    def run(self):
        signal.signal(signal.SIGTERM, lambda *_: setattr(self, 'stop_requested', True))
        signal.signal(signal.SIGINT, lambda *_: setattr(self, 'stop_requested', True))
        with (self.base / 'data' / 'recovery.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            private_write(self.base / 'data' / 'supervisor.pid', str(os.getpid()) + '\n')
            self.event('supervisor_started')
            try:
                while self.tick(time.monotonic()):
                    time.sleep(1)
            finally:
                if self.core is not None and self.core.poll() is None:
                    self.retire_core('supervisor_exit')
                    self.core.wait()  # Keep ownership until the child is actually gone.
                self.event('supervisor_stopped')


if __name__ == '__main__':
    try:
        base = Path(__file__).resolve().parent
        cfg = json.loads((base / 'config.json').read_text())
        if len(sys.argv) != 2 or cfg.get('desktop') is not True or cfg.get('web_listen') != '127.0.0.1:7576':
            raise RuntimeError('Desktop recovery configuration required')
        Supervisor(base, sys.argv[1], cfg).run()
    except Exception as error:
        print('Device recovery stopped: ' + type(error).__name__, file=sys.stderr, flush=True)
        sys.exit(1)
