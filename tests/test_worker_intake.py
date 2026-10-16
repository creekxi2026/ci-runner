"""Exercise the real pipe bridge; rejecting intake must run zero worker code."""
import importlib.util
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import unittest

ROOT = Path(__file__).resolve().parents[1]


def frame(kind, body):
    data = body.encode('utf-16-le')
    return struct.pack('<ii', kind, len(data)) + data


class WorkerIntake(unittest.TestCase):
    def run_bridge(self, reply, *, cancel=False):
        with tempfile.TemporaryDirectory(prefix='intake-') as tmp:
            tmp = Path(tmp)
            listener = socket.socket(socket.AF_UNIX)
            self.addCleanup(listener.close)
            listener.bind(str(tmp/'socket')); listener.listen(1)
            source_r, source_w = os.pipe(); output_r, output_w = os.pipe()
            for fd in (source_r, source_w, output_r, output_w): self.addCleanup(os.close, fd)
            sentinel = tmp/'ran'
            worker = tmp/'worker'
            worker.write_text('#!'+sys.executable+'\nimport os,sys\nopen('+repr(str(sentinel))+',"w").write("started")\n'
                'data=os.read(int(sys.argv[2]),65536)\nos.write(int(sys.argv[3]),data)\n')
            worker.chmod(0o700)
            code = ('import sys;sys.path.insert(0,'+repr(str(ROOT))+');import worker_intake;'
                    'sys.exit(worker_intake.bridge('+str(source_r)+','+str(output_w)+','+
                    repr(str(tmp/'socket'))+','+repr(str(worker))+',timeout=2))')
            proc = subprocess.Popen([sys.executable, '-c', code], pass_fds=(source_r, output_w),
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                job = json.dumps({'jobId':'7f5675bc-929c-4d5c-bf58-41f07d452abc',
                                  'variables':{'system.github.token':{'value':'test-only-token','isSecret':True}}})
                original = frame(1, job)
                os.write(source_w, original)
                listener.settimeout(3)
                conn, _ = listener.accept()
                with conn:
                    stream = conn.makefile('rb')
                    record = json.loads(stream.readline())
                    self.assertEqual(record['message']['variables']['system.github.token']['value'], 'test-only-token')
                    self.assertFalse(sentinel.exists(), 'candidate started before authentication')
                    if cancel:
                        os.write(source_w, frame(2, 'cancel'))
                        conn.settimeout(3)
                        self.assertEqual(conn.recv(1), b'', 'cancel failed to close token session')
                    else: conn.sendall(reply)
                    out, err = proc.communicate(timeout=5)
                self.assertNotIn(b'test-only-token', out+err)
                return proc.returncode, sentinel.exists(), original, os.read(output_r, 65536) if sentinel.exists() else b''
            finally:
                if proc.poll() is None: proc.kill(); proc.wait()

    def test_worker_receives_original_only_after_ready(self):
        code, ran, sent, got = self.run_bridge(b'0')
        self.assertEqual(code, 0)
        self.assertTrue(ran)
        self.assertEqual(got, sent)

    def test_rejected_intake_never_starts_worker(self):
        code, ran, _, _ = self.run_bridge(b'2')
        self.assertEqual(code, 2)
        self.assertFalse(ran)

    def test_cancellation_during_authentication_never_starts_worker(self):
        code, ran, _, _ = self.run_bridge(b'', cancel=True)
        self.assertEqual(code, 2)
        self.assertFalse(ran)


if __name__ == '__main__': unittest.main()
