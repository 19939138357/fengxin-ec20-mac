"""Subprocess fixture: no USB device, no network, no business sender."""
from pathlib import Path
from queue import Queue
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from vohive_mac_helper import ready, usb_stream

queue = Queue()
counter = Path(sys.argv[1])

class RX:
    def read(self, size, timeout):
        if size != 512 or timeout != 0:
            raise RuntimeError('Expected blocking single-packet input')
        value = queue.get()
        if isinstance(value, Exception):
            raise value
        return value

class TX:
    def write(self, payload, timeout):
        with counter.open('ab') as log:
            log.write(payload + b'\n')
        if payload == b'PARTIAL':
            return len(payload)-1
        if payload == b'FAIL':
            queue.put(OSError('simulated disconnected USB'))
        else:
            # Two separate packets: one full-size, then a short packet.
            queue.put(b'A' * 512)
            queue.put(b'REPLY:' + payload)
        return len(payload)

ready()
usb_stream(RX(), TX(), 512)
