"""One-shot Actions Runner 2.337.0 ProcessChannel bridge, before any worker code.

Only the image launcher calls this module. Inputs are inherited Listener pipes,
never workflow environment variables. The controller session has no reconnect or
candidate-controlled operation protocol. Payloads and tokens stay in memory.
"""
import ctypes
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


# Fixed stage names only; writes are nonblocking, best effort and bounded.
class Diagnostic:
    def __init__(self): self.count = 0
    def emit(self, stage, code=None):
        if os.environ.get('CI_INTAKE_DIAGNOSTICS') != '1' or self.count >= 32: return
        self.count += 1
        try:
            # stderr is launcher-owned before Worker starts. Do not change its
            # file status flags: the native Worker inherits this descriptor.
            if select.select([], [2], [], 0)[1]: os.write(2, ('ci-intake/v1 '+stage+((' code='+str(code)) if type(code) is int and -1 <= code <= 255 else '')+'\n').encode())
        except (OSError, ValueError): pass


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


def private_process():
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    # No privilege increase: prevent ordinary same-UID /proc access while this
    # process holds bootstrap material. Never rely on pipe permissions alone.
    if ctypes.CDLL(None, use_errno=True).prctl(4, 0, 0, 0, 0) != 0:
        raise ValueError('private bootstrap unavailable')


def receive_binding(fd, end):
    private_process()
    with socket.socket(fileno=fd) as broker:
        if broker.family != socket.AF_UNIX or broker.type != socket.SOCK_STREAM:
            raise ValueError('invalid bootstrap channel')
        broker.settimeout(max(.001, end-time.monotonic()))
        broker.sendall(b'1')
        material = bytearray(32)
        offset = 0
        try:
            while offset < len(material):
                count = broker.recv_into(memoryview(material)[offset:])
                if not count: raise ValueError('closed bootstrap')
                offset += count
            return material
        except BaseException:
            material[:] = bytes(len(material))
            raise


def bootstrap():
    # This supervisor is transient and allocation-local. It forks only the
    # trusted image startup chain, gives that chain an unopenable socketpair FD,
    # and serves the one capability request before permanently closing it.
    child = None
    left = right = None
    material = bytearray(32)
    diag = Diagnostic()
    def stop(*_): raise ValueError('terminated')
    try:
        private_process()
        diag.emit('bootstrap_started')
        for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP): signal.signal(sig, stop)
        end = time.monotonic()+600
        offset = 0
        while offset < len(material):
            left_time = end-time.monotonic()
            if left_time <= 0 or not select.select([0],[],[],left_time)[0]: return 2
            count = os.readv(0,[memoryview(material)[offset:]])
            if not count: return 2
            offset += count
        diag.emit('bootstrap_received')
        left, right = socket.socketpair()
        env = dict(os.environ, CI_INTAKE_BOOTSTRAP_FD=str(right.fileno()))
        child = subprocess.Popen([os.path.join(os.path.dirname(__file__), 'runner.sh')],
            env=env, stdin=subprocess.DEVNULL, pass_fds=(right.fileno(),), start_new_session=True)
        right.close(); right = None
        diag.emit('bootstrap_child_started')
        # Poll the child as well: a failed Listener must not leave a broker
        # retaining material for the entire initial admission window.
        while True:
            if child.poll() is not None: return child.returncode
            left_time = end-time.monotonic()
            if left_time <= 0: return 2
            if select.select([left], [], [], min(.1,left_time))[0]: break
        left.settimeout(max(.001,end-time.monotonic()))
        if left.recv(1) != b'1': return 2
        left.sendall(material)
        diag.emit('bootstrap_issued')
        material[:] = bytes(len(material))
        left.close(); left = None
        return child.wait()
    except (OSError, ValueError, subprocess.SubprocessError):
        return 2
    finally:
        material[:] = bytes(len(material))
        diag.emit('bootstrap_closed')
        for channel in (left,right):
            if channel is not None: channel.close()
        if child is not None:
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait()


def bridge(source, output, address, worker, *, timeout=600, binding=None):
    diag = Diagnostic()
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    end = time.monotonic()+timeout
    child = None
    channel = socket.socket(socket.AF_UNIX)
    def stop(*_): raise ValueError('terminated')
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP): signal.signal(sig, stop)
    try:
        os.fstat(source); os.fstat(output)
        diag.emit("fd_checked")
        diag.emit("frame_read")
        kind, body, original = message(source, end)
        if kind != 1: return 2
        raw = json.loads(body, object_pairs_hook=unique_object, parse_constant=invalid_constant)
        if not isinstance(raw, dict): return 2
        diag.emit("frame_parsed")
        channel.settimeout(max(.001, end-time.monotonic()))
        diag.emit("socket_connect")
        channel.connect(address)
        if binding is not None:
            try:
                write_all(channel.fileno(), binding, end, (source,))
                ready = select.select([source, channel], [], [], max(0,end-time.monotonic()))[0]
                if source in ready or channel not in ready: return 2
                channel.settimeout(max(.001,end-time.monotonic()))
                if channel.recv(1) != b'1': return 2
            finally: binding[:] = bytes(len(binding))
        diag.emit("socket_connected")
        payload = json.dumps(dict(schema='ci-job-intake/v1', message=raw), separators=(',', ':')).encode()+b'\n'
        if len(payload) > LIMIT: return 2
        write_all(channel.fileno(), payload, end, (source,))
        channel.settimeout(max(.001, end-time.monotonic()))
        del raw, body, payload
        # Cancellation is consumed before starting Worker, including while the
        # host is waiting for provider observations or its private assignment.
        diag.emit("host_wait")
        while True:
            left = end-time.monotonic()
            if left <= 0: return 2
            ready = select.select([source, channel], [], [], left)[0]
            if source in ready: return 2
            if channel in ready:
                if channel.recv(1) != b'0':
                    diag.emit('host_rejected'); return 2
                diag.emit('host_ready')
                break
        child_r, child_w = os.pipe()
        try:
            diag.emit("worker_start")
            child = subprocess.Popen([worker, 'spawnclient', str(child_r), str(output)],
                pass_fds=(child_r, output), start_new_session=True)
        finally: os.close(child_r)
        diag.emit("worker_started")
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
        diag.emit("bridge_rejected")
        return 2  # Never echo raw messages, API bodies or exception text.
    finally:
        if binding is not None: binding[:] = bytes(len(binding))
        diag.emit("session_closed")
        channel.close()
        if child is not None:
            # The group can outlive its leader, including on an early Worker
            # failure. Reap those descendants even after poll() reports exit.
            try: os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            child.wait()
            diag.emit("worker_exit", child.returncode)


def main():
    diag = Diagnostic()
    if len(sys.argv) == 2 and sys.argv[1] == 'bootstrap': return bootstrap()
    if len(sys.argv) != 4 or sys.argv[1] != 'spawnclient':
        diag.emit('cli_rejected'); return 2
    diag.emit('cli_checked')
    try:
        # Only this CLI requires startup binding. Direct bridge() calls remain
        # explicit test utilities, never an image launcher or admission route.
        fd = int(os.environ.pop('CI_INTAKE_BOOTSTRAP_FD'))
        binding = receive_binding(fd, time.monotonic()+30)
        try:
            return bridge(int(sys.argv[2]), int(sys.argv[3]), '/run/ci-intake/session.sock',
                          '/home/runner/bin/Runner.Worker.real', binding=binding)
        finally: binding[:] = bytes(len(binding))
    except (ValueError, OSError, KeyError): return 2


if __name__ == '__main__': raise SystemExit(main())
