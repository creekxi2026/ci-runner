"""Exercise the real pipe bridge; rejecting intake must run zero worker code."""
import importlib.util
from contextlib import contextmanager
import json
import os
from pathlib import Path
import socket
import signal
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]


def frame(kind, body):
    data = body.encode('utf-16-le')
    return struct.pack('<ii', kind, len(data)) + data


class WorkerIntake(unittest.TestCase):
    @contextmanager
    def session(self, body='{}', worker_body='import sys; sys.exit(19)', timeout=2):
        """Real process/pipe/socket fixture; no workflow or provider imitation."""
        with tempfile.TemporaryDirectory(prefix='intake-') as directory:
            tmp = Path(directory)
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(str(tmp/'socket')); listener.listen(1); listener.settimeout(3)
                source_r, source_w = os.pipe(); output_r, output_w = os.pipe()
                worker = tmp/'worker'
                worker.write_text('#!'+sys.executable+'\n'+worker_body+'\n')
                worker.chmod(0o700)
                code = ('import sys;sys.path.insert(0,'+repr(str(ROOT))+');import worker_intake;'
                        'sys.exit(worker_intake.bridge('+str(source_r)+','+str(output_w)+','+
                        repr(str(tmp/'socket'))+','+repr(str(worker))+',timeout='+str(timeout)+'))')
                proc = subprocess.Popen([sys.executable, '-c', code], pass_fds=(source_r, output_w),
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                os.close(source_r); os.close(output_w)
                def send():
                    try:
                        data = memoryview(frame(1, body))
                        while data: data = data[os.write(source_w, data):]
                    except BrokenPipeError: pass
                sender = threading.Thread(target=send)
                sender.start()
                try:
                    yield listener, proc, source_w, output_r
                finally:
                    if proc.poll() is None: proc.terminate()
                    proc.communicate(timeout=4)
                    sender.join(timeout=3)
                    self.assertFalse(sender.is_alive())
                    os.close(source_w); os.close(output_r)

    def test_ambiguous_or_non_object_json_never_reaches_adapter(self):
        for body in ('{"jobId":"a","jobId":"b"}', '{"variables":{"x":1,"x":2}}',
                     '{"jobId":"a","JobId":"b"}', '[]', 'null',
                     '{"value":NaN}', '{"value":Infinity}'):
            with self.subTest(body=body), self.session(body) as (listener, proc, _, _):
                self.assertEqual(proc.wait(timeout=3), 2)
                listener.settimeout(.05)
                try:
                    unexpected, _ = listener.accept()
                except socket.timeout:
                    continue
                unexpected.close()
                self.fail('unvalidated JSON reached adapter socket')

    def test_helper_can_wait_in_same_session_before_admitting(self):
        with self.session() as (listener, proc, _, _):
            with listener.accept()[0] as conn:
                record = json.loads(conn.makefile('rb').readline())
                self.assertEqual(record['schema'], 'ci-job-intake/v1')
                time.sleep(.15)
                self.assertIsNone(proc.poll())
                conn.sendall(b'0')
                self.assertEqual(proc.wait(timeout=3), 19)

    def test_revoke_interrupts_blocked_first_worker_delivery(self):
        # This exceeds pipe capacity. The child intentionally never reads it.
        child = 'import os,sys,time\nos.write(int(sys.argv[3]),b"started")\ntime.sleep(30)'
        with self.session('{"padding":"'+'x'*200000+'"}', child) as (listener, proc, _, output):
            with listener.accept()[0] as conn:
                json.loads(conn.makefile('rb').readline())
                conn.sendall(b'0')
                self.assertEqual(os.read(output, 7), b'started')
            self.assertEqual(proc.wait(timeout=3), 2)

    def test_cancel_interrupts_blocked_controller_delivery(self):
        with self.session('{"padding":"'+'x'*2000000+'"}', timeout=5) as (listener, proc, source, output):
            with listener.accept()[0] as conn:
                # The host does not read: intake's send exceeds socket capacity.
                time.sleep(.15)
                os.write(source, frame(2, ''))
                self.assertEqual(proc.wait(timeout=2), 2)
                self.assertEqual(os.read(output, 1), b'')

    def test_duplicate_job_after_admission_revokes_session(self):
        child = 'import os,sys,time\nos.write(int(sys.argv[3]),b"started")\ntime.sleep(30)'
        with self.session(worker_body=child) as (listener, proc, source, output):
            with listener.accept()[0] as conn:
                conn.makefile('rb').readline(); conn.sendall(b'0')
                self.assertEqual(os.read(output, 7), b'started')
                os.write(source, frame(1, '{}'))
                self.assertEqual(proc.wait(timeout=3), 2)
                self.assertEqual(conn.recv(1), b'')

    def test_cancel_revokes_before_forwarding_and_preserves_worker_exit(self):
        child = '''import os,sys,struct
def exact(n):
    data=b''
    while len(data)<n: data+=os.read(int(sys.argv[2]),n-len(data))
    return data
kind,size=struct.unpack('<ii',exact(8)); exact(size)
os.write(int(sys.argv[3]),b'started')
kind,size=struct.unpack('<ii',exact(8)); exact(size)
os.write(int(sys.argv[3]),str(kind).encode())
sys.exit(103)
'''
        for kind in (2, 3, 4):
            with self.subTest(kind=kind), self.session(worker_body=child) as (listener, proc, source, output):
                with listener.accept()[0] as conn:
                    conn.makefile('rb').readline(); conn.sendall(b'0')
                    self.assertEqual(os.read(output, 7), b'started')
                    os.write(source, frame(kind, ''))
                    conn.settimeout(3)
                    self.assertEqual(conn.recv(1), b'')
                    self.assertEqual(os.read(output, 1), str(kind).encode())
                    self.assertEqual(proc.wait(timeout=3), 103)

    def test_helper_exit_or_authentication_deadline_does_not_admit(self):
        for close in (True, False):
            with self.subTest(close=close), self.session(timeout=.3) as (listener, proc, _, output):
                with listener.accept()[0] as conn:
                    conn.makefile('rb').readline()
                    if close: conn.shutdown(socket.SHUT_RDWR)
                    self.assertEqual(proc.wait(timeout=2), 2)
                    self.assertEqual(os.read(output, 1), b'')

    def test_worker_exit_reaps_remaining_process_group(self):
        child = '''import os,sys,struct,subprocess
def exact(n):
    data=b''
    while len(data)<n: data+=os.read(int(sys.argv[2]),n-len(data))
    return data
kind,size=struct.unpack('<ii',exact(8)); exact(size)
p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'])
os.write(int(sys.argv[3]),str(p.pid).encode())
sys.exit(19)
'''
        with self.session(worker_body=child) as (listener, proc, _, output):
            with listener.accept()[0] as conn:
                conn.makefile('rb').readline(); conn.sendall(b'0')
                descendant = int(os.read(output, 32))
                try:
                    # Descendants inherit stdout/stderr. EOF proves group teardown.
                    proc.communicate(timeout=2)
                    self.assertEqual(proc.returncode, 19)
                finally:
                    try: os.kill(descendant, signal.SIGKILL)
                    except ProcessLookupError: pass

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
