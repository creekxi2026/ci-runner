"""One-shot Actions Runner 2.337.0 ProcessChannel bridge, before any worker code.

Only the image launcher calls this module. Inputs are inherited Listener pipes,
never workflow environment variables. The controller session has no reconnect or
candidate-controlled operation protocol. Payloads and tokens stay in memory.
"""
import json
import os
import resource
import select
import signal
import socket
import struct
import subprocess
import sys
import time

LIMIT = 8 * 1024 * 1024


def exact(fd, size, end):
    data = bytearray()
    while len(data) < size:
        left = end - time.monotonic()
        if left <= 0 or not select.select([fd], [], [], left)[0]: raise ValueError('deadline')
        chunk = os.read(fd, size-len(data))
        if not chunk: raise ValueError('closed channel')
        data.extend(chunk)
    return bytes(data)


def message(fd, end):
    header = exact(fd, 8, end)
    kind, size = struct.unpack('<ii', header)
    if kind not in (1, 2, 3, 4) or not 0 <= size <= LIMIT or size % 2: raise ValueError('invalid frame')
    body = exact(fd, size, end)
    return kind, body.decode('utf-16-le'), header+body


def write_all(fd, data):
    view = memoryview(data)
    while view:
        size = os.write(fd, view)
        if size <= 0: raise ValueError('closed channel')
        view = view[size:]


def bridge(source, output, address, worker, *, timeout=600):
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    end = time.monotonic()+timeout
    child = None
    channel = socket.socket(socket.AF_UNIX)
    def stop(*_): raise ValueError('terminated')
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP): signal.signal(sig, stop)
    try:
        kind, body, original = message(source, end)
        if kind != 1: return 2
        raw = json.loads(body)
        channel.settimeout(max(.001, end-time.monotonic()))
        channel.connect(address)
        payload = json.dumps(dict(schema='ci-job-intake/v1', message=raw), separators=(',', ':')).encode()+b'\n'
        if len(payload) > LIMIT: return 2
        channel.sendall(payload)
        del raw, body, payload
        # Cancellation is consumed before starting Worker, including while the
        # host is waiting for provider observations or its private assignment.
        while True:
            left = end-time.monotonic()
            if left <= 0: return 2
            ready = select.select([source, channel], [], [], left)[0]
            if source in ready: return 2
            if channel in ready:
                if channel.recv(1) != b'0': return 2
                break
        child_r, child_w = os.pipe()
        try:
            child = subprocess.Popen([worker, 'spawnclient', str(child_r), str(output)],
                pass_fds=(child_r, output), start_new_session=True)
        finally: os.close(child_r)
        try:
            write_all(child_w, original)
            del original
            while child.poll() is None:
                ready = select.select([source, channel], [], [], .1)[0]
                if channel in ready:
                    # The host closes/revokes on expiry, provider rejection or
                    # controller exit. No candidate can extend this session.
                    return 2
                if source in ready:
                    kind, _, frame = message(source, time.monotonic()+5)
                    if kind not in (2, 3, 4): return 2
                    channel.close()  # revoke before forwarding cancellation
                    write_all(child_w, frame)
                    return child.wait(timeout=10)
            return child.returncode
        finally: os.close(child_w)
    except (OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError):
        return 2  # Never echo raw messages, API bodies or exception text.
    finally:
        channel.close()
        if child is not None and child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait()


def main():
    if len(sys.argv) != 4 or sys.argv[1] != 'spawnclient': return 2
    try:
        return bridge(int(sys.argv[2]), int(sys.argv[3]), '/run/ci-intake/session.sock',
                      '/home/runner/bin/Runner.Worker.real')
    except (ValueError, OSError): return 2


if __name__ == '__main__': raise SystemExit(main())
