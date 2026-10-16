# Worker intake protocol and offline evidence

This interface is opt-in under `ci-job-inputs/v2`. It does not register a runner,
authenticate a project, or install an adapter. The adapter and controller's
assignment remain responsible for provider identity and project authorization.

## Fixed upstream and entry points

The Dockerfile pins the Linux ARM64 Actions Runner **2.337.0** archive with
SHA256 `9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393`.
The upstream source commit is `397b032cbf865e9c3ddfab89d533ec19325e1273`:

- [JobDispatcher](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Listener/JobDispatcher.cs) spawns `Runner.Worker spawnclient <input-fd> <output-fd>`.
- [ProcessChannel](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Common/ProcessChannel.cs) sends type 1 (NewJob), then types 2/3/4 (cancel/runner shutdown/OS shutdown).
- [StreamString](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Common/StreamString.cs) encodes little-endian int32 type and byte length followed by UTF-16LE text on ARM64.
- [Worker](https://github.com/actions/runner/blob/397b032cbf865e9c3ddfab89d533ec19325e1273/src/Runner.Worker/Worker.cs) consumes that inherited channel.

Before Listener runs, `runner.sh` renames the original apphost to
`Runner.Worker.real` and installs the image-owned Python interposer. The CLI is
`python3 /opt/ci/worker_intake.py spawnclient <input-fd> <output-fd>`; its controller
address is fixed at `/run/ci-intake/session.sock`. Neither path is a workflow input.
The Worker assemblies and .NET runtime remain unmodified. An upstream upgrade
requires new executable evidence, not just new synthetic frame fixtures.

## Messages, identities and admission

The first frame must be type 1, valid UTF-16LE JSON, at most 8 MiB. Its root must
be an object. Duplicate keys (including case variants), non-finite numbers,
malformed/deeply recursive JSON and invalid lengths fail closed before IPC.
The JSON-line IPC envelope is bounded separately to 8 MiB:

```json
{"schema":"ci-job-intake/v1","message":{"jobId":"<upstream GUID>","requestId":123}}
```

`message` is the original parsed allocation object, including any upstream
variables/resources. It is not a new credential format. The controller supplies
its own private directory and forwards
`{schema:"ci-job-input-session/v1",directory,message}` to the digest-pinned
command. The installed adapter must match job/run/attempt/runner identities to
the controller assignment and live provider; a candidate-provided jobId, token,
environment variable or file alone grants nothing. The generic layer never
invents a session ID or interprets project token/attempt fields. The single-use
allocation socket plus the live helper process defines the session lifetime.

Only the helper's first stdout byte `0` admits Worker. It must publish its gate
before that byte. The exact original type-1 frame, rather than reserialized
JSON, then reaches Worker. Further NewJob frames revoke the session. The Go
controller accepts one connection and unlinks its name; there is no replay,
reconnect or candidate-controlled renewal route. Each later assigned job needs
its own socket, original allocation and freshly authenticated helper.

## Waiting, exits and cancellation

| Boundary | Meaning |
| --- | --- |
| Project check returns 0 | That requested check succeeded; the helper may acknowledge only after all admission conditions/gate publication succeed. |
| Project check returns 75 | Bounded waiting; the same helper must retry/wait before its first stdout byte, honoring cancellation and the earliest token/evidence deadline. |
| Project check returns 2 | Refusal/failure; no Worker admission. |
| Helper exits before `0`, even with exit 0 or 75 | No admission. The socket closes and the proxy fails with 2. The generic bridge never reruns a helper or renews its token. |
| Worker exits | Its exit status reaches Listener unchanged for ordinary exit codes, including upstream job-result codes. Proxy failures are 2. |

The proxy's initial admission deadline is 600 seconds; the controller's socket
session hard limit is one hour. These are outer bounds, not extensions of an
adapter's shorter token/evidence/work deadline. Waiting inside a single helper
is compatible with the one-shot socket; exiting 75 and expecting a reconnect is
not compatible. The existing v1 hook's timeout is not a v2 retry mechanism.

Both outbound IPC and first Worker-frame delivery are nonblocking and observe
revocation/cancel input. The first Worker frame has a 30-second delivery bound.
Cancellation before admission or during a partial first-frame delivery aborts
the proxy without admitting additional candidate code. After full delivery,
types 2/3/4 close the private session **before** forwarding the original frame;
forwarding has a five-second bound and Worker has ten seconds to exit. Expiry,
controller exit, invalid frames and proxy signals revoke/kill the Worker group.
Group cleanup also runs after the Worker leader has already exited, so its
remaining descendants cannot retain pipes. Host-side disconnect kills the
helper group; controller reclamation removes the job's private socket/subpath.
Raw allocations, credentials and helper errors are not logged or persisted by
the bridge. Core dumps are disabled.

## Repeatable offline checks

Run `go test -race ./...`, `go vet ./...`, and
`python3 -m unittest discover -s tests -v` in their supported environments.
Ownership tests require an isolated Linux root process with the existing
CHOWN/DAC_OVERRIDE/FOWNER capability contract. Git cache tests require a real
filesystem with at least 5 GiB free; use an owned subdirectory of the fixed work
volume, not a new volume or a small tmpfs. Tests must clean that subdirectory.

`tests/native_worker_probe.py` is an explicit offline executable probe. Run it
as UID1001 in the pinned runner image, with network disabled, all capabilities
dropped, a disposable writable `/tmp` and disposable writable
`/opt/actions-runner/_diag`. Mount this repository read-only. Compile the Go
test executable for Linux ARM64 with the pinned Go 1.26.9 toolchain and expose it
read-only through `CI_NATIVE_INTAKE_TEST_BINARY`; the probe then exercises actual
`runIntake`, a digest-pinned **fixture** adapter, the Python interposer, and the
unmodified renamed .NET Worker. It checks binary/assembly hashes and the actual
Worker version/commit, receipt and deserialization trace. It also checks that a
refusal starts no Worker and leaves no socket. No provider or project authority
is claimed by the fixture adapter.

The probe deliberately supplies an incomplete credential-free job. Native
deserialization must succeed and the later native job validation must fail with
exit 1, propagated by the proxy. This proves the local executable transport and
failure boundary, not a successful Actions job. Listener can separately run
`--version` offline; its server allocation and JobDispatcher lifecycle require
an authorized real platform run. Native successful/failed/cancelled job status,
server result publication and exact installed project adapters remain separate
integration/onsite evidence. No echo Worker or skipped workflow substitutes for
those results.
