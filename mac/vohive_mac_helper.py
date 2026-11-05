#!/usr/bin/env python3
"""Private pipe helpers: EC20 USB AT, and 8 kHz mono PCM16 Core Audio."""
import fcntl
import json
import os
from pathlib import Path
import sys
import threading

BASE = Path(__file__).resolve().parent
sys.path.insert(0, str(BASE / 'vendor'))
os.umask(0o077)


def ready():
    print('READY', file=sys.stderr, flush=True)


def usb_stream(rx, tx, packet_size):
    """Read complete USB packets without repeatedly cancelling idle reads."""
    def fail(error):
        print(f'{type(error).__name__}: {error}', file=sys.stderr, flush=True)
        os._exit(1)

    def receive():
        try:
            while True:
                data = bytes(rx.read(packet_size, timeout=0))
                if data:
                    sys.stdout.buffer.write(data)
                    sys.stdout.buffer.flush()
        except BrokenPipeError:
            os._exit(0)
        except Exception as error:
            fail(error)

    threading.Thread(target=receive, daemon=True, name='EC20-USB-reader').start()
    try:
        while True:
            payload = os.read(sys.stdin.fileno(), 4096)
            if not payload:
                # An infinite USB read must not race libusb finalizers. This is
                # a private pipe child: exit releases its handles and lock.
                os._exit(0)
            if tx.write(payload, timeout=2000) != len(payload):
                raise RuntimeError('Partial USB write; command result is uncertain')
    except Exception as error:
        fail(error)


def usb_pipe(device, control_lines=False):
    import libusb_package
    import usb.core
    import usb.util
    if device != 'usb:2c7c:0125':
        raise ValueError('Unsupported USB device; expected usb:2c7c:0125')
    # Shared with the standalone EC20 tool. An idle CLI menu does not own it.
    lock_path = BASE.parent / 'EC20-Mac' / 'data' / 'modem.lock'
    if not lock_path.parent.is_dir():
        raise RuntimeError('Missing shared EC20-Mac/data directory')
    with lock_path.open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError('EC20 is in use; stop the other EC20/VoHive tool first')
        devices = list(libusb_package.find(find_all=True, idVendor=0x2c7c, idProduct=0x0125))
        if len(devices) != 1:
            raise RuntimeError(f'Expected exactly one EC20; found {len(devices)}')
        dev = devices[0]
        try:
            interface = dev.get_active_configuration()[(2, 0)]
            bulk = [e for e in interface if usb.util.endpoint_type(e.bmAttributes) == usb.util.ENDPOINT_TYPE_BULK]
            rx = next(e for e in bulk if usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_IN)
            tx = next(e for e in bulk if usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_OUT)
            usb.util.claim_interface(dev, 2)
            if control_lines:
                # Optional standard serial open handshake, for a scoped probe.
                if dev.ctrl_transfer(0x21, 0x22, 3, 2, b'', timeout=2000) != 0:
                    raise RuntimeError('USB serial control-line setup incomplete')
            # Never reset USB or detach its system audio driver.
            packet_size = rx.wMaxPacketSize & 0x7ff
            if packet_size <= 0:
                raise RuntimeError('Invalid bulk input packet size')
            ready()
            usb_stream(rx, tx, packet_size)
        finally:
            usb.util.dispose_resources(dev)


def audio_ids(device, sd):
    names = device.split('|')
    if len(names) != 2 or not all(names):
        raise ValueError('Audio selection requires exact input|output names')
    ids = []
    for name, direction in zip(names, ('input', 'output')):
        found = [i for i, d in enumerate(sd.query_devices())
                 if d['name'] == name and d[f'max_{direction}_channels'] >= 1]
        if len(found) != 1:
            raise RuntimeError(f'Expected exactly one {direction} endpoint {name!r}; found {len(found)}')
        ids.append(found[0])
    return ids


def audio_pipe(mode, device):
    import sounddevice as sd
    input_id, output_id = audio_ids(device, sd)
    kwargs = dict(samplerate=8000, channels=1, dtype='int16', blocksize=160)
    if mode == 'capture':
        with sd.RawInputStream(device=input_id, **kwargs) as stream:
            ready()
            while True:
                data, overflowed = stream.read(160)
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
    elif mode == 'playback':
        with sd.RawOutputStream(device=output_id, **kwargs) as stream:
            ready()
            while True:
                data = sys.stdin.buffer.read(320)
                if not data:
                    return
                if len(data) != 320:
                    raise RuntimeError('Truncated PCM frame')
                stream.write(data)
    elif mode == 'probe':
        # Only the selected EC20 endpoints. No default Mac microphone/speaker.
        with sd.RawInputStream(device=input_id, **kwargs) as capture:
            with sd.RawOutputStream(device=output_id, **kwargs) as playback:
                total = 0
                for _ in range(10):
                    playback.write(bytes(320))
                    raw, _ = capture.read(160)
                    total += len(raw)
        print(json.dumps({'input': sd.query_devices(input_id)['name'],
                          'output': sd.query_devices(output_id)['name'],
                          'sample_rate': 8000, 'channels': 1, 'format': 'int16',
                          'captured_bytes': total, 'telephony_audio_verified': False}))
    else:
        raise ValueError('Unknown audio mode')


if __name__ == '__main__':
    try:
        mode, device = sys.argv[1:]
        if mode == 'devices':
            from desktop_audio import devices
            print(json.dumps(devices(), ensure_ascii=False))
        elif mode == 'desktop':
            from desktop_audio import bridge
            bridge(json.loads(device))
        elif mode in ('usb', 'usb-active'):
            usb_pipe(device, control_lines=mode == 'usb-active')
        else:
            audio_pipe(mode, device)
    except (BrokenPipeError, KeyboardInterrupt):
        pass
    except Exception as exc:
        print(f'{type(exc).__name__}: {exc}', file=sys.stderr, flush=True)
        sys.exit(1)
