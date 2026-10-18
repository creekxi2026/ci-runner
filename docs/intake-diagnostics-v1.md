# Intake diagnostics: v2 controller records, v1 producer lines

`CI_INTAKE_DIAGNOSTICS=1` enables these optional observations. The default is off.
The controller passes that boolean to its helper and v2 worker. Diagnostic output
never grants admission, authenticates a peer, retries a job or changes a deadline.

## Controller record contract

The controller emits exactly this ordered, space-separated envelope (its existing
logger may prefix a date/time):

```
ci-intake/v2 job=ci-job-example source=controller stage=atomic_published segment=<controller-process-UUID> seq=7 elapsed_ns=123456 clock=controller evidence=observed
```

`job` is the controller-selected allocation name, matching the existing bounded
service identifier. `source` is controller, helper or worker. `stage` must match
that source's fixed allowlist in `intake_diagnostics.go`; unknown schemas, sources,
stages and appended producer fields are dropped. Exit records alone may append
`code=-1` or a decimal integer 0..255. No payload, field value, exception text, URL,
header, credential, credential digest or arbitrary producer field is recorded.

`segment` is a fresh random public identifier for one controller process, unrelated
to credentials. `seq` is process-wide and strictly increases across emitted
records. `elapsed_ns` measures time since that process began; its sample, sequence
increment and output share one lock. Restart creates a new segment and resets the
sequence/clock. Records in different segments cannot establish ordering or elapsed
time between controller processes. No new persistent registry is introduced.

`clock=controller` always describes the recording clock. `evidence=observed` is
reserved for the controller's own local operations. Helper/worker records carry
`evidence=forwarded`: their sequence/time is collection order/time, **not** the
producer clock, source event order, or authenticated proof of execution. Missing
records remain unknown (off, lost, truncated, logging failure or unreached).
Consumers must check the exact schema and source/stage/evidence combination;
no diagnostic record is a trusted release receipt or a successful job result.

## Scheduler and durability observations

The handler confirms a current allocation through the live fleet state, its
allocation mutex, a retained spec and validated immutable assignment identity.
It excludes allocations already cleaning. That mutex also serializes publication
with cleanup. An unknown runner, an old spec alone, or a nil return from scheduler
journal observation does not establish current allocation membership.

- `started_observed`: this handler is processing a started event associated with
  that allocation. A repeated event in another message may produce another local
  observation; a fully processed message retry produces none. It does not prove
  the service's original event order or that Worker executed.
- `completed_fallback`: this handler actually takes the completed-event assignment
  compensation branch because assignment is absent. If started already supplied
  the assignment, including earlier in the same batch, no fallback is recorded.
- `atomic_published`: assignment rename **and** parent directory fsync succeeded
  on this call. Immutable equal-file returns, errors and unknown durability never
  emit a new publication. If rename succeeds and directory fsync fails, the file
  may exist; a later equal-file return does not invent a publication observation.

Started observation may precede a failed write; fallback may precede an unsuccessful
publication. Without `atomic_published` durability stays unknown. A controller
restart does not reclassify existing immutable files as new publications. These
records only describe local handler/persistence facts; they cannot explain a
historical incident solely from file mtime or a later experiment.

## Legacy producer and collection boundaries

Helper and worker emit `ci-intake/v1 <stage>` on stderr. Worker completion may emit
`ci-intake/v1 worker_exit code=<integer>` (-1 or 0..255). Their wire format is
unchanged. The controller filters the entire line against separate helper/worker
allowlists and re-emits only safe v2 forwarded records. A helper cannot emit
controller `atomic_published` or worker `frame_parsed`; worker stderr cannot emit
helper `provider_ok`. `token_present` describes field shape, not provider approval.

Helper stderr retains its streaming 96-byte line bound and 64-record cap; raw
stderr is discarded. Each worker-side producer emits at most 32 records; the existing collector cap
also bounds the combined startup/bridge stream. Cleanup reads Docker stderr
tail 128 through the existing socket before deletion: one second, 64KiB total,
8KiB framing, at most 64 accepted lines. Invalid framing and collector failure are
best effort and cannot block normal rejection/cleanup. Native JobDispatcher may
consume Worker stderr internally, leaving no Docker stderr record even when the
bridge ran; collection is not a complete execution trace.

Controller logs retain the deployment's existing rotation. Export safe records
before retiring that controller, then remove evidence after investigation
acceptance. No new volume, endpoint, service, permission or journal is introduced.
The current project helper emits v1 only; no project parser uses these records to
write trusted state. Project provider checks, helper gate-before-ready and native
release receipts remain separate. See [Worker protocol](worker-intake-protocol.md)
for the actual one-shot intake and cancellation contract.

`session_bound` / `session_rejected` are controller observations of the separate
startup capability handshake. Worker `bootstrap_started`, `bootstrap_received`,
`bootstrap_child_started`, `bootstrap_issued` and `bootstrap_closed` are bounded
forwarded observations, with no material or credential digest. Neither source
proves an exact OS process identity.
