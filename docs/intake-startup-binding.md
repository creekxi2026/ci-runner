# Allocation startup session binding

The v2 intake authenticates possession of a one-use capability delivered to this
allocation's trusted image startup chain. This is **startup session binding**,
not authentication of a particular OS process, PID, UID, job payload or diagnostic
line. The controller's existing private Docker API identity and image/spec
validation remain the trust boundary. An ordinary same-UID socket client cannot
obtain admission by presenting a valid-looking NewJob message.

## Existing-channel handshake

1. During prepare, the controller creates 32 random bytes for this allocation in
   memory only. Separate issuance and verification buffers expire after 600
   seconds. The Docker spec contains only public `OpenStdin`, `AttachStdin` and
   `StdinOnce` flags; there is no secret environment variable, file or spec field.
2. Before starting the container, the controller attaches the existing Docker
   HTTP Upgrade stdin stream, then starts the container and writes the material
   once. This upgrade has its own 30-second context bound; the ordinary Docker
   client's timeout remains unchanged. Partial/failed writes fail closed.
3. Before copying any candidate workspace or starting Listener, the image launcher
   runs a transient allocation-local broker. It disables core dumps and sets
   Linux `PR_SET_DUMPABLE=0` before reading stdin directly into a mutable buffer.
   It launches the original runner script in a child process group. Only an
   anonymous Unix socketpair endpoint and its public FD number are inherited
   through that trusted script and native Listener startup. Listener never holds
   the bootstrap material itself.
4. The image-owned interposer obtains the one response from this inherited
   endpoint. It also disables dumps before receiving the material. It connects
   to the existing allocation socket, sends exactly 32 bytes and requires a
   one-byte controller acknowledgement before sending the unchanged v1 JSON
   envelope. The controller atomically consumes verification authority on first
   accept, compares in constant time, and acknowledges only on a valid unexpired
   allocation binding. An unauthenticated packet cannot start the helper.
5. Both sides clear their mutable buffers on success, error, expiry or shutdown.
   The broker closes its capability endpoint after serving once. It remains a
   transient supervisor only to reap its startup process group; it has no network
   listener, reconnect API, durable credential or reusable verification service.

The original helper gate-before-ready, provider checks, immutable assignment,
single accepted controller socket, original Worker delivery and disconnect /
cancellation / one-hour session lifetime remain in force. The original Worker
inherits only its native channel descriptors, not the broker endpoint or the
controller session connection. Restart cannot recover prior material from disk;
there is no supported reconnect or renewal of an old allocation.

## Limits and verification

This relies on an intact trusted image startup chain running before candidate
code and on the existing no-capabilities, no-new-privileges container boundary.
It does not protect against the controller/Docker administrator, image tampering,
a compromised trusted startup executable, or an actor already controlling that
chain. An unrelated process with the same UID receives no capability merely by
sharing that UID. Linux `/proc` cannot reopen an anonymous socket endpoint as a
file; the non-dumpable broker additionally prevents ordinary sibling access to
its stdin, environment and memory. No host PID lookup, ptrace, added privilege,
new port or persistent broker is used by the implementation.

Unit and integration checks cover unbound JSON, wrong allocation, expiry,
repeated issuance, consumed authority, controller restart and cancellation. The
external local acceptance fixture exercises real Docker stdin, sibling PID
namespaces, UID1001, the pinned native Listener JobDispatcher/ProcessChannel /
ProcessInvoker and original Worker. Its provider/configuration IO is synthetic;
this proves the local startup/protocol chain, not native GitHub authentication,
a live release, or a historical incident's cause. Safe diagnostic records remain
observations only. See [diagnostics](intake-diagnostics-v1.md).
