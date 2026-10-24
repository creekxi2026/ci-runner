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
A new diagnostic controller cannot be combined with an old worker revision under
that contract. Consuming projects also retain their exact installed lane image
contracts; an operator overlay cannot override them without a separately reviewed
new source/input contract.
