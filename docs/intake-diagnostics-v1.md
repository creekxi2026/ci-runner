# Intake stage diagnostics v1

`CI_INTAKE_DIAGNOSTICS=1` on the controller enables fixed stage observations. The
controller passes that boolean to its helper and v2 worker. No policy schema,
credential source, admission rule, deadline or ready-byte protocol changes.

Worker/helper producers emit `ci-intake/v1 <stage>` on stderr. Worker completion
may emit `ci-intake/v1 worker_exit code=<integer>` (-1 or 0..255). Stages are the
literal allowlist in `intake_diagnostics.go`; arbitrary text and appended fields
are rejected. Controller records contain only `job`, `source` (controller,
helper, worker), `stage`, and optional numeric `code`. Job is controller selected.
No payload, field values, exception text, URL, header or credential digest is
recorded. `token_present` means validated field shape, never provider acceptance.

Helper stderr is streamed through a 96-byte line filter, at most 64 records.
Raw helper stderr is discarded. Worker emits at most 32 records. Before deleting
an owned runner, the controller reads stderr tail 128 through the existing Docker
socket (one-second bound, 64KiB total, 8KiB framing limit), retains at most 64 safe
records in its existing controller log, then performs normal cleanup. Failures
are best effort and never change rejection or resource cleanup. No new volume,
service, endpoint or permissions are needed. Controller log retention follows the
current deployment log rotation; retain/export the safe log before retiring that
controller, and remove exported evidence after the investigation is accepted.
Historical journals are unchanged. Worker messages are advisory observations,
never trusted identity or evidence of admission: job code could forge them.

Interpret the producer chain separately: prepare -> runner entry -> interposer
CLI/FD -> frame -> socket connect/accept -> packet -> assignment wait -> checked helper
-> installation/scope -> identity/provider -> gate -> ready -> Worker -> exit.
The helper must publish its existing gate before writing `0`. Streams drain
asynchronously, so their display order is not a cross-process clock. An absent
record is unknown (disabled, dropped, truncated, failed sink, or unreached), not a
reason to infer token, FD, provider or Worker failure. Replaying a token or socket
is still forbidden. Native Listener allocation and provider identity require
separate native evidence; offline diagnostics do not establish a native cause.

`assignment_wait` follows packet validation; `assignment_ready` precedes helper
execution. `assignment_unavailable` means this wait ended without readiness
(including cancellation/disconnect), not proof of a scheduler timeout. Scheduler
publication and Listener delivery are independent; assignment can arrive first
or later. A completed-event fallback can also publish assignment during cleanup,
so file mtime alone cannot identify the scheduler event that produced it.

Native JobDispatcher subscribes to ProcessInvoker's stderr events and collects
Worker output internally. The bridge's stderr stages may therefore be absent
from Docker stderr even when it connected successfully. Collection timestamps
are not the original worker emission timestamps. The offline native probe can
explicitly forward fixed stages; production does not change Listener assemblies.
