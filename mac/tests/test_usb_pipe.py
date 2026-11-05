from pathlib import Path
import select
import subprocess
import sys
import tempfile
import unittest


class PipeTests(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[2] / '.build/test-tmp'
        root.mkdir(parents=True, exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(prefix='pipe-', dir=root)
        self.counter = Path(self.directory.name) / 'writes.bin'
        self.child = subprocess.Popen([sys.executable, str(Path(__file__).with_name('fake_usb_pipe.py')), str(self.counter)],
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(self.child.stderr.readline(), b'READY\n')

    def tearDown(self):
        if self.child.poll() is None:
            self.child.stdin.close()
            self.child.wait(timeout=3)
        for pipe in [self.child.stdin, self.child.stdout, self.child.stderr]:
            if not pipe.closed: pipe.close()
        self.directory.cleanup()

    def test_input_blocks_without_delaying_write_and_keeps_full_packets(self):
        self.child.stdin.write(b'AT\r')
        self.child.stdin.flush()
        self.assertTrue(select.select([self.child.stdout], [], [], 2)[0])
        expected = b'A' * 512 + b'REPLY:AT\r'
        self.assertEqual(self.child.stdout.read(len(expected)), expected)
        self.assertEqual(self.counter.read_bytes(), b'AT\r\n')
        self.child.stdin.close()
        self.assertEqual(self.child.wait(timeout=3), 0)

    def test_receive_error_closes_an_idle_writer_promptly(self):
        self.child.stdin.write(b'FAIL')
        self.child.stdin.flush()
        self.assertEqual(self.child.wait(timeout=3), 1)
        self.assertEqual(self.counter.read_bytes(), b'FAIL\n')

    def test_partial_write_is_never_replayed(self):
        self.child.stdin.write(b'PARTIAL')
        self.child.stdin.flush()
        self.assertEqual(self.child.wait(timeout=3), 1)
        self.assertEqual(self.counter.read_bytes(), b'PARTIAL\n')


if __name__ == '__main__':
    unittest.main()
