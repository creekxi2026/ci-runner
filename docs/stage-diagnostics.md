# Optional typed stage diagnostics

The controller accepts `CI_DIAGNOSTIC_PROFILE` only as an opt-in private read-only
JSON file containing window_id, source_revision, image_digest and scope_ref. Its
sibling `salt` must contain 32 random bytes, mode 0600. Both files must be regular,
single-link files owned by the effective UID, or by the explicitly configured
`CI_DIAGNOSTIC_OWNER_UID` for an operator-owned read-only host bind. The revision
must match the compiled controller. Invalid input disables diagnostics; the
operator must treat missing diagnostics as incomplete evidence and stop its
window. Ordinary controllers have no profile or salt.

Output is bounded `ci-stage-v1` NDJSON on stdout. Raw existing controller stderr
is not part of this evidence. The consumer must accept only the closed schema,
trusted identity, fixed enums/counts, 2 KiB lines and monotonic sequence. No URL,
error text, credentials, response body or raw session ID can enter the typed sink.
Session references use a per-window secret HMAC-SHA256 salt, never an unkeyed hash.

The local patched SDK and controller emit logical begin/end events and HTTP retry
attempts separately. A missing end means only that it was not captured. Queue
loss, missing shutdown summary, read errors, rotation/gaps, archive caps or a
forced stop make evidence incomplete. Diagnostics never prove business success,
native run terminal state or resource release.

The sink has a bounded channel and separate writer goroutine. Writer panic,
short write, I/O failure or a blocked writer cannot block the business producer.
Final Close has a two-second caller budget; a physically blocked writer can
remain until process exit. No logger callback executes under SDK refresh locks;
only typed construction, HMAC and enqueue run in those paths.

The complete local upstream SDK snapshot and source/delta hashes live in
third_party/scaleset. Its nil-default option does not change transports, timeouts,
retry results, error chains or refresh lock positions. Root ./... excludes the
nested module: run `go -C third_party/scaleset test -race ./...` and vet explicitly.
Image preflight verifies the local source closure before Docker builds.

This candidate does not authorize image publication, deployment or an observer
window. Existing matching controller/runner revision enforcement remains intact.
A new diagnostic controller requires a worker built from the same revision.
An isolated observer candidate can declare a new pair through the consuming
project's existing config and overlay contracts; this does not change ordinary
controllers, old installations or their evidence. Runtime labels and actual
digests still require the normal build and image checks. Immutable installation
directories also need an accepted, unused executor version; never overwrite an
old directory or manufacture an empty commit to change its name.

`TestDiagnosticStartResourceTraceMatrix` compares the real provisioning and
cleanup calls with diagnostics disabled, enabled and with failed, short, panicking
or blocked writers. It covers reserve/capacity, network, proxy, JIT, disk, worker,
firewall, final release, cancellation and deadline paths. The finite transport
creates no Docker resources. `TestDiagnosticBootstrapDemandRetryEquivalence`
compares the bootstrap/demand retry, polling and acknowledgement path.

The nested module's `TestDiagnosticRefreshConcurrencyMatrix` starts twelve
callers together, verifies auth cache/refresh and session duplicate-refresh
counts, and compares requests and return/error chains. Concurrent request order
is a scheduler choice, so that test compares multisets plus exact endpoint
counts; the serial SDK test preserves its ordered trace comparison. All secrets
in test traces are explicit fixture baits, never real credentials. Diagnostic
output is separately checked for those baits and raw session identity.

Diagnostic profile and salt inputs use nonblocking, nofollow opens and validate
the opened descriptor's regular-file type, owner, link count, mode and size.
A FIFO without a writer disables diagnostics immediately, so a bad optional
input cannot strand the controller before its normal configuration checks.
`TestDiagnosticInputTypesBounded` exercises both inputs in bounded child
processes, including FIFO, directory, symlink, hardlink, mode and size rejection.
