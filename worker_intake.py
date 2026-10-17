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


def write_all(fd, data, end, interrupts=()):
    # A Worker can exit or stop reading before consuming a full NewJob frame.
    # Never block cancellation/revocation behind the capacity of its pipe.
    os.set_blocking(fd, False)
    view = memoryview(data)
    while view:
        left = end-time.monotonic()
        if left <= 0: raise ValueError('deadline')
        ready, writable, _ = select.select(interrupts, [fd], [], min(left, .1))
        if ready: raise ValueError('revoked channel')
        if not writable: continue
        try: size = os.write(fd, view)
        except BlockingIOError: continue
        if size <= 0: raise ValueError('closed channel')
        view = view[size:]


def unique_object(pairs):
    result = {}
    folded = set()
    for key, value in pairs:
        # The downstream .NET typed JSON properties are case insensitive.
        if key.casefold() in folded: raise ValueError('duplicate field')
        folded.add(key.casefold())
        result[key] = value
    return result


def invalid_constant(_): raise ValueError('invalid JSON constant')


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
        raw = json.loads(body, object_pairs_hook=unique_object, parse_constant=invalid_constant)
        if not isinstance(raw, dict): return 2
        channel.settimeout(max(.001, end-time.monotonic()))
        channel.connect(address)
        payload = json.dumps(dict(schema='ci-job-intake/v1', message=raw), separators=(',', ':')).encode()+b'\n'
        if len(payload) > LIMIT: return 2
        write_all(channel.fileno(), payload, end, (source,))
        channel.settimeout(max(.001, end-time.monotonic()))
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
            write_all(child_w, original, time.monotonic()+30, (source, channel))
            del original
            while child.poll() is None:
                ready = select.select([source, channel], [], [], .1)[0]
                if child.poll() is not None: return child.returncode
                if channel in ready:
                    # The host closes/revokes on expiry, provider rejection or
                    # controller exit. No candidate can extend this session.
                    return 2
                if source in ready:
                    kind, _, frame = message(source, time.monotonic()+5)
                    if kind not in (2, 3, 4): return 2
                    channel.close()  # revoke before forwarding cancellation
                    write_all(child_w, frame, time.monotonic()+5)
                    return child.wait(timeout=10)
            return child.returncode
        finally: os.close(child_w)
    except (OSError, ValueError, TypeError, KeyError, RecursionError, OverflowError, subprocess.SubprocessError):
        return 2  # Never echo raw messages, API bodies or exception text.
    finally:
        channel.close()
        if child is not None:
            # The group can outlive its leader, including on an early Worker
            # failure. Reap those descendants even after poll() reports exit.
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait()


def main():
    if len(sys.argv) != 4 or sys.argv[1] != 'spawnclient': return 2
    try:
        return bridge(int(sys.argv[2]), int(sys.argv[3]), '/run/ci-intake/session.sock',
                      '/home/runner/bin/Runner.Worker.real')
    except (ValueError, OSError): return 2


if __name__ == '__main__': raise SystemExit(main())
