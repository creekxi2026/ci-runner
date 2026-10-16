"""Offline probe of the pinned, unmodified .NET Worker, not an Actions job.

Run explicitly in the runner image: python3 tests/native_worker_probe.py.
No token, registration, network, Docker socket or persistent volume is used.
The intentionally incomplete assignment must fail AFTER native deserialization.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import struct
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
RUNNER = Path('/opt/actions-runner')
EXPECTED = {
    'Runner.Worker': '3dc89dfbfb2198a9c366ee55a7a5473ca72d288d4c35ea76b27495bc7450fac3',
    'Runner.Worker.dll': '882978326b71cd4fc8ccc62c6010806462b65ca034b058574aac48d486803c7e',
    'Runner.Common.dll': 'a91434c075d381a1375a890719a3a59f75f3b3902f3545e460b13aeccd9e82f3',
}


def main():
    assert os.getuid() == 1001, 'probe must use ordinary worker UID'
    for name, digest in EXPECTED.items():
        assert hashlib.sha256((RUNNER/'bin'/name).read_bytes()).hexdigest() == digest, name
    results = []
    with tempfile.TemporaryDirectory(prefix='native-intake-') as temporary:
        root = Path(temporary)
        for reply in (b'2', b'0'):
            home = root/reply.decode(); (home/'bin').mkdir(parents=True)
            for item in (RUNNER/'bin').iterdir():
                (home/'bin'/item.name).symlink_to(item)
            # Same apphost rename as runner.sh; all assemblies remain unmodified.
            worker = home/'bin/Runner.Worker.real'
            shutil.copy2(RUNNER/'bin/Runner.Worker', worker)
            address = str(home/'intake.sock')
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(address); listener.listen(1); listener.settimeout(5)
                server = None
                native_binary = os.environ.get('CI_NATIVE_INTAKE_TEST_BINARY')
                if native_binary:
                    listener.close()
                    os.unlink(address)
                    server = subprocess.Popen([native_binary, '-test.run=^TestNativeWorkerProbeServer$', '-test.timeout=20s'],
                        env=dict(os.environ, CI_NATIVE_PROBE_SOCKET=address, CI_NATIVE_PROBE_REPLY=reply.decode()),
                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    assert server.stdout.readline() == b'listening\n'
                source_r, source_w = os.pipe(); output_r, output_w = os.pipe()
                code = ('import sys;sys.path.insert(0,'+repr(str(ROOT))+');import worker_intake;'
                        'sys.exit(worker_intake.bridge('+str(source_r)+','+str(output_w)+','+
                        repr(address)+','+repr(str(worker))+',timeout=5))')
                proc = subprocess.Popen([sys.executable, '-c', code], pass_fds=(source_r, output_w),
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                os.close(source_r); os.close(output_w)
                try:
                    # No endpoint or credential. This is a local transport probe.
                    body = json.dumps({'requestId':131, 'jobId':'7f5675bc-929c-4d5c-bf58-41f07d452abc',
                                       'resources':{}, 'variables':{}})
                    wire = body.encode('utf-16-le')
                    os.write(source_w, struct.pack('<ii', 1, len(wire))+wire)
                    if server:
                        out, err = proc.communicate(timeout=15)
                        server_out, server_err = server.communicate(timeout=5)
                        assert server.returncode == 0, (server_out, server_err)
                        assert not Path(address).exists(), 'server socket survived'
                    else:
                        with listener.accept()[0] as conn:
                            with conn.makefile('rb') as stream:
                                packet = json.loads(stream.readline())
                                assert packet == {'schema':'ci-job-intake/v1', 'message':json.loads(body)}
                                assert not list((RUNNER/'_diag').glob('Worker_*.log')), 'Worker started before ready'
                                conn.sendall(reply)
                                out, err = proc.communicate(timeout=15)
                    logs = '\n'.join(p.read_text() for p in (RUNNER/'_diag').glob('Worker_*.log'))
                    if reply == b'2':
                        assert proc.returncode == 2 and not logs and not out and not err
                    else:
                        assert proc.returncode == 1, (proc.returncode, out, err)
                        for marker in ('Version: 2.337.0', 'Commit: 397b032cbf865e9c3ddfab89d533ec19325e1273',
                                       'Message received.', 'Job message:', 'requestId'):
                            assert marker in logs, (marker, logs)
                        assert '131' in logs
                        # A real Worker failure propagates, rather than becoming 0.
                        assert 'Exception' in logs
                    results.append({'reply':reply.decode(), 'proxy_exit':proc.returncode,
                                    'native_log_bytes':len(logs), 'native_consumed':reply == b'0',
                                    'native_trace':logs})
                finally:
                    if proc.poll() is None: proc.terminate()
                    proc.communicate(timeout=4)
                    if server:
                        if server.poll() is None: server.terminate()
                        server.communicate(timeout=4)
                    os.close(source_w); os.close(output_r)
    print(json.dumps({'uid':os.getuid(), 'assemblies':EXPECTED, 'results':results,
                      'go_intake':bool(os.environ.get('CI_NATIVE_INTAKE_TEST_BINARY')),
                      'scope':'offline transport and native failure; no server assignment or job lifecycle'}, indent=2))


if __name__ == '__main__': main()
